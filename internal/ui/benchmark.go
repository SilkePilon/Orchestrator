package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

const (
	benchmarkNamespace                = "default"
	benchmarkImage                    = "alpine:3.20"
	benchmarkDiskSizeMiB              = 128
	benchmarkJobTTLSeconds            = 300
	benchmarkGracePeriodSeconds       = 5
	benchmarkIperfPort          int32 = 5201
	benchmarkCPUMaxPrime              = 20000
)

var benchmarkNameRe = regexp.MustCompile(`[^a-z0-9-]+`)
var benchmarkThreadsRe = regexp.MustCompile(`orchestrator_threads=([0-9]+)`)
var benchmarkEventsRe = regexp.MustCompile(`events per second:\s*([0-9]+(?:\.[0-9]+)?)`)
var benchmarkTotalTimeRe = regexp.MustCompile(`total time:\s*([0-9]+(?:\.[0-9]+)?)s`)
var benchmarkMemorySpeedRe = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)\s+MiB/sec\)`)

type fioJSONResult struct {
	Jobs []struct {
		Read  fioJSONDirection `json:"read"`
		Write fioJSONDirection `json:"write"`
	} `json:"jobs"`
}

type fioJSONDirection struct {
	BWBytes int64   `json:"bw_bytes"`
	IOPS    float64 `json:"iops"`
	LatNS   struct {
		Mean float64 `json:"mean"`
	} `json:"lat_ns"`
}

type iperfJSONResult struct {
	End struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int64   `json:"retransmits"`
		} `json:"sum_sent"`
		SumReceived struct {
			BitsPerSecond float64 `json:"bits_per_second"`
		} `json:"sum_received"`
	} `json:"end"`
}

type BenchmarkView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx context.Context

	target       *adw.ComboRow
	targetModel  *gtk.StringList
	cpuBench     *adw.SwitchRow
	memoryBench  *adw.SwitchRow
	diskBench    *adw.SwitchRow
	networkBench *adw.SwitchRow
	apiBench     *adw.SwitchRow
	resourceScan *adw.SwitchRow
	podDensity   *adw.SwitchRow
	samples      *adw.SpinRow
	runButton    *gtk.Button
	saveButton   *gtk.Button
	status       *gtk.Label
	progress     *gtk.ProgressBar
	page         *adw.PreferencesPage
	resultGroups []*adw.PreferencesGroup
	runCancel    context.CancelFunc

	nodes []corev1.Node
	last  *benchmarkResult
}

type benchmarkResult struct {
	Version    int                   `json:"version"`
	Cluster    string                `json:"cluster"`
	Target     string                `json:"target"`
	Options    benchmarkOptions      `json:"options"`
	StartedAt  time.Time             `json:"startedAt"`
	FinishedAt time.Time             `json:"finishedAt"`
	APILatency *benchmarkLatency     `json:"apiLatency,omitempty"`
	Nodes      []benchmarkNodeResult `json:"nodes"`
}

type benchmarkOptions struct {
	CPUPerformance bool `json:"cpuPerformance"`
	MemorySpeed    bool `json:"memorySpeed"`
	DiskSpeed      bool `json:"diskSpeed"`
	NetworkSpeed   bool `json:"networkSpeed"`
	APILatency     bool `json:"apiLatency"`
	ResourceScan   bool `json:"resourceScan"`
	PodDensity     bool `json:"podDensity"`
	LatencySamples int  `json:"latencySamples"`
}

type benchmarkProgressFunc func(done, total int, message string)

type benchmarkLatency struct {
	Samples int     `json:"samples"`
	MinMS   float64 `json:"minMs"`
	AvgMS   float64 `json:"avgMs"`
	MaxMS   float64 `json:"maxMs"`
	Errors  int     `json:"errors"`
}

type benchmarkNodeResult struct {
	Name                   string                  `json:"name"`
	Ready                  bool                    `json:"ready"`
	CPUCapacityMilli       int64                   `json:"cpuCapacityMilli"`
	CPUAllocatableMilli    int64                   `json:"cpuAllocatableMilli"`
	CPUUsageMilli          *int64                  `json:"cpuUsageMilli,omitempty"`
	MemoryCapacityBytes    int64                   `json:"memoryCapacityBytes"`
	MemoryAllocatableBytes int64                   `json:"memoryAllocatableBytes"`
	MemoryUsageBytes       *int64                  `json:"memoryUsageBytes,omitempty"`
	PodCapacity            int64                   `json:"podCapacity"`
	PodCount               *int                    `json:"podCount,omitempty"`
	PowerDrawWatts         *float64                `json:"powerDrawWatts,omitempty"`
	PowerSource            string                  `json:"powerSource"`
	CPUBenchmark           *benchmarkCPUResult     `json:"cpuBenchmark,omitempty"`
	MemoryBenchmark        *benchmarkMemoryResult  `json:"memoryBenchmark,omitempty"`
	DiskBenchmark          *benchmarkDiskResult    `json:"diskBenchmark,omitempty"`
	NetworkBenchmark       *benchmarkNetworkResult `json:"networkBenchmark,omitempty"`
}

type benchmarkCPUResult struct {
	Image            string  `json:"image"`
	Threads          int     `json:"threads,omitempty"`
	EventsPerSecond  float64 `json:"eventsPerSecond,omitempty"`
	TotalTimeSeconds float64 `json:"totalTimeSeconds,omitempty"`
	WallTimeSeconds  float64 `json:"wallTimeSeconds,omitempty"`
	CPUMaxPrime      int     `json:"cpuMaxPrime"`
	Error            string  `json:"error,omitempty"`
}

type benchmarkMemoryResult struct {
	Image            string  `json:"image"`
	Threads          int     `json:"threads,omitempty"`
	MiBPerSecond     float64 `json:"mibPerSecond,omitempty"`
	TotalTimeSeconds float64 `json:"totalTimeSeconds,omitempty"`
	WallTimeSeconds  float64 `json:"wallTimeSeconds,omitempty"`
	Error            string  `json:"error,omitempty"`
}

type benchmarkDiskResult struct {
	Image                    string  `json:"image"`
	SizeMiB                  int     `json:"sizeMiB"`
	SequentialReadMiBPerSec  float64 `json:"sequentialReadMiBPerSec,omitempty"`
	SequentialWriteMiBPerSec float64 `json:"sequentialWriteMiBPerSec,omitempty"`
	RandomReadMiBPerSec      float64 `json:"randomReadMiBPerSec,omitempty"`
	RandomReadIOPS           float64 `json:"randomReadIops,omitempty"`
	RandomReadLatencyUS      float64 `json:"randomReadLatencyUs,omitempty"`
	RandomWriteMiBPerSec     float64 `json:"randomWriteMiBPerSec,omitempty"`
	RandomWriteIOPS          float64 `json:"randomWriteIops,omitempty"`
	RandomWriteLatencyUS     float64 `json:"randomWriteLatencyUs,omitempty"`
	WallTimeSeconds          float64 `json:"wallTimeSeconds,omitempty"`
	Error                    string  `json:"error,omitempty"`
}

type benchmarkNetworkResult struct {
	Image           string  `json:"image"`
	PeerNode        string  `json:"peerNode,omitempty"`
	Role            string  `json:"role,omitempty"`
	MbitsPerSecond  float64 `json:"mbitsPerSecond,omitempty"`
	Retransmits     int64   `json:"retransmits,omitempty"`
	WallTimeSeconds float64 `json:"wallTimeSeconds,omitempty"`
	Error           string  `json:"error,omitempty"`
}

func NewBenchmarkView(ctx context.Context, state *common.ClusterState) *BenchmarkView {
	view := &BenchmarkView{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	title := adw.NewWindowTitle("Benchmark", state.ClusterPreferences.Value().Name)
	header.SetTitleWidget(title)

	view.runButton = gtk.NewButtonWithLabel("Run")
	view.runButton.AddCSSClass("suggested-action")
	view.runButton.ConnectClicked(view.onRunButtonClicked)
	header.PackEnd(view.runButton)

	view.saveButton = gtk.NewButtonFromIconName("document-save-symbolic")
	view.saveButton.AddCSSClass("flat")
	view.saveButton.SetTooltipText("Save benchmark results")
	view.saveButton.SetSensitive(false)
	view.saveButton.ConnectClicked(view.save)
	header.PackEnd(view.saveButton)

	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)
	view.page = page

	options := adw.NewPreferencesGroup()
	options.SetTitle("Benchmark Options")
	options.SetDescription("Runs Kubernetes checks and optional short-lived benchmark workloads on the selected node or nodes. CPU and memory usage come from metrics-server when available.")
	page.Add(options)

	view.targetModel = gtk.NewStringList([]string{"All nodes"})
	view.target = adw.NewComboRow()
	view.target.SetTitle("Target")
	view.target.SetModel(view.targetModel)
	options.Add(view.target)

	view.cpuBench = adw.NewSwitchRow()
	view.cpuBench.SetTitle("CPU performance")
	view.cpuBench.SetSubtitle("Creates a temporary pod and runs sysbench on the actual node CPU")
	view.cpuBench.SetActive(true)
	options.Add(view.cpuBench)

	view.memoryBench = adw.NewSwitchRow()
	view.memoryBench.SetTitle("Memory bandwidth")
	view.memoryBench.SetSubtitle("Runs a temporary sysbench memory test on the selected node or nodes")
	view.memoryBench.SetActive(true)
	options.Add(view.memoryBench)

	view.diskBench = adw.NewSwitchRow()
	view.diskBench.SetTitle("Disk I/O")
	view.diskBench.SetSubtitle("Runs Jeff-style sequential and 4K random fio tests on node-local ephemeral storage")
	view.diskBench.SetActive(false)
	options.Add(view.diskBench)

	view.networkBench = adw.NewSwitchRow()
	view.networkBench.SetTitle("Network throughput")
	view.networkBench.SetSubtitle("Runs temporary iperf3 tests between nodes, similar to Jeff's cluster network checks")
	view.networkBench.SetActive(false)
	options.Add(view.networkBench)

	view.apiBench = adw.NewSwitchRow()
	view.apiBench.SetTitle("API latency")
	view.apiBench.SetSubtitle("Measures Kubernetes API list latency over several samples")
	view.apiBench.SetActive(true)
	options.Add(view.apiBench)

	view.resourceScan = adw.NewSwitchRow()
	view.resourceScan.SetTitle("Node resources")
	view.resourceScan.SetSubtitle("Shows capacity, allocatable CPU/memory, and current usage")
	view.resourceScan.SetActive(true)
	options.Add(view.resourceScan)

	view.podDensity = adw.NewSwitchRow()
	view.podDensity.SetTitle("Pod density")
	view.podDensity.SetSubtitle("Counts scheduled pods compared with each node's pod capacity")
	view.podDensity.SetActive(true)
	options.Add(view.podDensity)

	view.samples = adw.NewSpinRow(gtk.NewAdjustment(5, 1, 50, 1, 5, 0), 1, 0)
	view.samples.SetTitle("Latency samples")
	options.Add(view.samples)

	resultsGroup := adw.NewPreferencesGroup()
	resultsGroup.SetTitle("Status")
	page.Add(resultsGroup)

	view.status = gtk.NewLabel("Run a benchmark to see results.")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.SetWrap(true)
	view.status.AddCSSClass("dim-label")
	resultsGroup.Add(view.status)

	view.progress = gtk.NewProgressBar()
	view.progress.SetShowText(true)
	view.progress.SetVisible(false)
	view.progress.AddCSSClass("benchmark-progress")
	view.progress.SetVAlign(gtk.AlignCenter)
	view.progress.SetMarginTop(6)
	view.progress.SetMarginBottom(6)
	resultsGroup.Add(view.progress)

	view.refreshNodes()
	return view
}

func (v *BenchmarkView) refreshNodes() {
	var nodes corev1.NodeList
	if err := v.Cluster.List(v.ctx, &nodes); err != nil {
		v.status.SetText(fmt.Sprintf("Could not load nodes: %s", err))
		return
	}
	v.nodes = nodes.Items
	v.targetModel.Splice(0, v.targetModel.NItems(), []string{"All nodes"})
	for _, node := range v.nodes {
		v.targetModel.Append(node.Name)
	}
}

func (v *BenchmarkView) onRunButtonClicked() {
	if v.runCancel != nil {
		v.cancelRun()
		return
	}
	v.run()
}

func (v *BenchmarkView) setRunButtonRunning(running bool) {
	if running {
		v.runButton.SetLabel("Cancel")
		v.runButton.RemoveCSSClass("suggested-action")
		v.runButton.AddCSSClass("destructive-action")
		v.runButton.SetTooltipText("Cancel the running benchmark")
	} else {
		v.runButton.SetLabel("Run")
		v.runButton.RemoveCSSClass("destructive-action")
		v.runButton.AddCSSClass("suggested-action")
		v.runButton.SetTooltipText("")
	}
}

func (v *BenchmarkView) cancelRun() {
	if v.runCancel == nil {
		return
	}
	v.status.SetText("Cancelling benchmark…")
	v.runButton.SetSensitive(false)
	v.runCancel()
}

func (v *BenchmarkView) run() {
	runCtx, cancel := context.WithCancel(v.ctx)
	v.runCancel = cancel
	v.setRunButtonRunning(true)
	v.saveButton.SetSensitive(false)
	v.status.SetText("Running benchmark…")
	v.progress.SetVisible(true)
	v.progress.SetFraction(0)
	v.progress.SetText("Preparing")
	v.clearResultGroups()

	target := "All nodes"
	if selected := v.target.Selected(); selected > 0 && selected-1 < uint(len(v.nodes)) {
		target = v.nodes[selected-1].Name
	}
	v.refreshNodes()
	options := benchmarkOptions{
		CPUPerformance: v.cpuBench.Active(),
		MemorySpeed:    v.memoryBench.Active(),
		DiskSpeed:      v.diskBench.Active(),
		NetworkSpeed:   v.networkBench.Active(),
		APILatency:     v.apiBench.Active(),
		ResourceScan:   v.resourceScan.Active(),
		PodDensity:     v.podDensity.Active(),
		LatencySamples: int(v.samples.Value()),
	}

	report := func(done, total int, message string) {
		glib.IdleAdd(func() {
			v.status.SetText(message)
			v.progress.SetVisible(true)
			if total > 0 {
				v.progress.SetFraction(min(float64(done)/float64(total), 1))
				v.progress.SetText(fmt.Sprintf("%d / %d", done, total))
			} else {
				v.progress.Pulse()
				v.progress.SetText("Preparing")
			}
		})
	}

	go func() {
		result, err := v.collectBenchmark(runCtx, target, options, report)
		var history []benchmarkResult
		var historyErr error
		var saveErr error
		if err == nil {
			history, historyErr = v.loadBenchmarkHistory()
			saveErr = v.saveBenchmarkHistory(result)
		}
		glib.IdleAdd(func() {
			cancel()
			v.runCancel = nil
			v.setRunButtonRunning(false)
			v.runButton.SetSensitive(true)
			if err != nil {
				if errors.Is(err, context.Canceled) || runCtx.Err() == context.Canceled {
					v.status.SetText("Benchmark cancelled.")
					v.progress.SetText("Cancelled")
					v.progress.SetFraction(0)
					return
				}
				v.status.SetText("Benchmark failed.")
				v.progress.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Benchmark failed", err)
				return
			}
			v.last = result
			v.saveButton.SetSensitive(true)
			v.status.SetText(fmt.Sprintf("Finished %s", result.FinishedAt.Format("15:04:05")))
			v.progress.SetFraction(1)
			v.progress.SetText("Done")
			v.renderResult(result)
			if historyErr == nil {
				v.renderBenchmarkHistory(result, history)
			}
			if saveErr != nil {
				widget.ShowErrorDialog(v.ctx, "Could not save benchmark history", saveErr)
			}
		})
	}()
}

func (v *BenchmarkView) collectBenchmark(parent context.Context, target string, options benchmarkOptions, report benchmarkProgressFunc) (*benchmarkResult, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()

	started := time.Now()
	result := &benchmarkResult{
		Version:   1,
		Cluster:   v.ClusterPreferences.Value().Name,
		Target:    target,
		Options:   options,
		StartedAt: started,
	}
	report(0, 0, "Loading selected nodes...")
	selectedNodes, err := v.selectedNodes(ctx, target)
	if err != nil {
		return nil, err
	}

	totalSteps := benchmarkStepTotal(options, len(selectedNodes))
	doneSteps := 0
	startStep := func(message string) {
		report(doneSteps, totalSteps, message)
	}
	finishStep := func(message string) {
		doneSteps++
		report(doneSteps, totalSteps, message)
	}

	if options.APILatency {
		startStep("Measuring Kubernetes API latency...")
		latency, err := v.measureAPILatency(ctx, options.LatencySamples)
		if err != nil {
			return nil, err
		}
		result.APILatency = latency
		finishStep("Measured Kubernetes API latency")
	}

	metricsByNode := map[string]metricsv1beta1.NodeMetrics{}
	if options.ResourceScan {
		startStep("Reading node CPU and memory usage...")
		var nodeMetrics metricsv1beta1.NodeMetricsList
		if err := v.Cluster.List(ctx, &nodeMetrics); err == nil {
			for _, metrics := range nodeMetrics.Items {
				metricsByNode[metrics.Name] = metrics
			}
		}
		finishStep("Read node CPU and memory usage")
	}

	podCounts := map[string]int{}
	if options.PodDensity {
		startStep("Counting scheduled pods per node...")
		var pods corev1.PodList
		if err := v.Cluster.List(ctx, &pods); err != nil {
			return nil, err
		}
		for _, pod := range pods.Items {
			if pod.Spec.NodeName != "" && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				podCounts[pod.Spec.NodeName]++
			}
		}
		finishStep("Counted scheduled pods")
	}

	for _, node := range selectedNodes {
		nodeResult := benchmarkNodeResult{
			Name:                   node.Name,
			Ready:                  nodeReady(node),
			CPUCapacityMilli:       node.Status.Capacity.Cpu().MilliValue(),
			CPUAllocatableMilli:    node.Status.Allocatable.Cpu().MilliValue(),
			MemoryCapacityBytes:    node.Status.Capacity.Memory().Value(),
			MemoryAllocatableBytes: node.Status.Allocatable.Memory().Value(),
			PodCapacity:            node.Status.Capacity.Pods().Value(),
			PowerSource:            "not exposed by Kubernetes metrics-server",
		}
		if metrics, ok := metricsByNode[node.Name]; ok {
			if cpu := metrics.Usage.Cpu(); cpu != nil {
				value := cpu.MilliValue()
				nodeResult.CPUUsageMilli = &value
			}
			if memory := metrics.Usage.Memory(); memory != nil {
				value := memory.Value()
				nodeResult.MemoryUsageBytes = &value
			}
		}
		if options.PodDensity {
			count := podCounts[node.Name]
			nodeResult.PodCount = &count
		}
		if options.CPUPerformance {
			startStep(fmt.Sprintf("Running CPU performance on %s...", node.Name))
			nodeResult.CPUBenchmark = v.runNodeCPUBenchmark(ctx, node)
			finishStep(fmt.Sprintf("Finished CPU performance on %s", node.Name))
		}
		if options.MemorySpeed {
			startStep(fmt.Sprintf("Running memory bandwidth on %s...", node.Name))
			nodeResult.MemoryBenchmark = v.runNodeMemoryBenchmark(ctx, node)
			finishStep(fmt.Sprintf("Finished memory bandwidth on %s", node.Name))
		}
		if options.DiskSpeed {
			startStep(fmt.Sprintf("Running disk I/O on %s...", node.Name))
			nodeResult.DiskBenchmark = v.runNodeDiskBenchmark(ctx, node)
			finishStep(fmt.Sprintf("Finished disk I/O on %s", node.Name))
		}
		result.Nodes = append(result.Nodes, nodeResult)
	}
	if options.NetworkSpeed {
		v.runNetworkBenchmarks(ctx, selectedNodes, result.Nodes, &doneSteps, totalSteps, report)
	}

	result.FinishedAt = time.Now()
	return result, nil
}

func (v *BenchmarkView) measureAPILatency(ctx context.Context, samples int) (*benchmarkLatency, error) {
	if samples < 1 {
		samples = 1
	}
	latency := &benchmarkLatency{Samples: samples, MinMS: math.MaxFloat64}
	var total float64
	for i := 0; i < samples; i++ {
		started := time.Now()
		var nodes corev1.NodeList
		if err := v.Cluster.List(ctx, &nodes); err != nil {
			latency.Errors++
			continue
		}
		elapsed := float64(time.Since(started).Microseconds()) / 1000
		latency.MinMS = min(latency.MinMS, elapsed)
		latency.MaxMS = max(latency.MaxMS, elapsed)
		total += elapsed
	}
	validSamples := samples - latency.Errors
	if validSamples == 0 {
		return nil, fmt.Errorf("all API latency samples failed")
	}
	latency.AvgMS = total / float64(validSamples)
	return latency, nil
}

func (v *BenchmarkView) selectedNodes(ctx context.Context, target string) ([]corev1.Node, error) {
	var nodes corev1.NodeList
	if err := v.Cluster.List(ctx, &nodes); err != nil {
		return nil, err
	}
	if target == "All nodes" {
		return nodes.Items, nil
	}
	for _, node := range nodes.Items {
		if node.Name == target {
			return []corev1.Node{node}, nil
		}
	}
	return nil, fmt.Errorf("node %q was not found", target)
}

func (v *BenchmarkView) runNodeCPUBenchmark(ctx context.Context, node corev1.Node) *benchmarkCPUResult {
	result := &benchmarkCPUResult{Image: benchmarkImage, CPUMaxPrime: benchmarkCPUMaxPrime}
	script := fmt.Sprintf(`set -eu
apk add --no-cache sysbench >/dev/null
threads="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 1)"
echo "orchestrator_threads=$threads"
sysbench cpu --threads="$threads" --time=10 --cpu-max-prime=%d run
`, result.CPUMaxPrime)
	logs, wallTime, err := v.runNodeBenchmarkJob(ctx, node, "cpu", script)
	result.WallTimeSeconds = wallTime
	if err != nil {
		result.Error = err.Error()
	}
	parseCPUBenchmarkLogs(result, logs)
	if result.EventsPerSecond == 0 && result.Error == "" {
		result.Error = "sysbench did not report an events-per-second score"
	}
	return result
}

func (v *BenchmarkView) runNodeMemoryBenchmark(ctx context.Context, node corev1.Node) *benchmarkMemoryResult {
	result := &benchmarkMemoryResult{Image: benchmarkImage}
	script := `set -eu
apk add --no-cache sysbench >/dev/null
threads="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 1)"
echo "orchestrator_threads=$threads"
sysbench memory --threads="$threads" --time=10 --memory-block-size=1M --memory-total-size=100G run
`
	logs, wallTime, err := v.runNodeBenchmarkJob(ctx, node, "memory", script)
	result.WallTimeSeconds = wallTime
	if err != nil {
		result.Error = err.Error()
	}
	parseMemoryBenchmarkLogs(result, logs)
	if result.MiBPerSecond == 0 && result.Error == "" {
		result.Error = "sysbench did not report memory bandwidth"
	}
	return result
}

func (v *BenchmarkView) runNodeDiskBenchmark(ctx context.Context, node corev1.Node) *benchmarkDiskResult {
	result := &benchmarkDiskResult{Image: benchmarkImage, SizeMiB: benchmarkDiskSizeMiB}
	script := fmt.Sprintf(`set -eu
apk add --no-cache fio >/dev/null
cd /bench
echo ORCHESTRATOR_FIO_SEQ_READ_BEGIN
fio --name=orchestrator-seq-read --filename=orchestrator-fio-seq-read --size=%dM --rw=read --bs=1M --iodepth=16 --numjobs=1 --runtime=10 --time_based --direct=1 --group_reporting --output-format=json
echo ORCHESTRATOR_FIO_SEQ_READ_END
echo ORCHESTRATOR_FIO_SEQ_WRITE_BEGIN
fio --name=orchestrator-seq-write --filename=orchestrator-fio-seq-write --size=%dM --rw=write --bs=1M --iodepth=16 --numjobs=1 --runtime=10 --time_based --direct=1 --group_reporting --output-format=json
echo ORCHESTRATOR_FIO_SEQ_WRITE_END
echo ORCHESTRATOR_FIO_READ_BEGIN
fio --name=orchestrator-read --filename=orchestrator-fio-read --size=%dM --rw=randread --bs=4k --iodepth=16 --numjobs=1 --runtime=10 --time_based --direct=1 --group_reporting --output-format=json
echo ORCHESTRATOR_FIO_READ_END
echo ORCHESTRATOR_FIO_WRITE_BEGIN
fio --name=orchestrator-write --filename=orchestrator-fio-write --size=%dM --rw=randwrite --bs=4k --iodepth=16 --numjobs=1 --runtime=10 --time_based --direct=1 --group_reporting --output-format=json
echo ORCHESTRATOR_FIO_WRITE_END
rm -f orchestrator-fio-*
`, result.SizeMiB, result.SizeMiB, result.SizeMiB, result.SizeMiB)
	logs, wallTime, err := v.runNodeBenchmarkJob(ctx, node, "disk", script)
	result.WallTimeSeconds = wallTime
	if err != nil {
		result.Error = err.Error()
	}
	parseDiskBenchmarkLogs(result, logs)
	if result.SequentialReadMiBPerSec == 0 && result.SequentialWriteMiBPerSec == 0 && result.RandomReadMiBPerSec == 0 && result.RandomWriteMiBPerSec == 0 && result.Error == "" {
		result.Error = "fio did not report disk throughput"
	}
	return result
}

func (v *BenchmarkView) runNetworkBenchmarks(ctx context.Context, nodes []corev1.Node, results []benchmarkNodeResult, doneSteps *int, totalSteps int, report benchmarkProgressFunc) {
	startStep := func(message string) {
		report(*doneSteps, totalSteps, message)
	}
	finishStep := func(message string) {
		*doneSteps++
		report(*doneSteps, totalSteps, message)
	}

	if len(nodes) < 2 {
		startStep("Checking network benchmark target nodes...")
		for i := range results {
			results[i].NetworkBenchmark = &benchmarkNetworkResult{Image: benchmarkImage, Error: "network benchmark needs at least two selected nodes"}
		}
		finishStep("Skipped network throughput")
		return
	}

	server := nodes[0]
	serverName := benchmarkJobName("iperf", server.Name)
	labels := benchmarkLabels(serverName)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: benchmarkNamespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			NodeName:                      server.Name,
			TerminationGracePeriodSeconds: int64Ptr(benchmarkGracePeriodSeconds),
			Containers: []corev1.Container{{
				Name:            "iperf3",
				Image:           benchmarkImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-c", "apk add --no-cache iperf3 >/dev/null && iperf3 -s"},
				Ports:           []corev1.ContainerPort{{ContainerPort: benchmarkIperfPort}},
			}},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: benchmarkNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "iperf3",
				Port:       benchmarkIperfPort,
				TargetPort: intstr.FromInt32(benchmarkIperfPort),
			}},
		},
	}

	startStep(fmt.Sprintf("Starting iperf3 server on %s...", server.Name))
	if _, err := v.Cluster.CoreV1().Pods(benchmarkNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		setNetworkError(results, fmt.Sprintf("create iperf3 server: %s", err))
		finishStep(fmt.Sprintf("Could not start iperf3 server on %s", server.Name))
		return
	}
	defer v.deleteBenchmarkPod(serverName)
	if _, err := v.Cluster.CoreV1().Services(benchmarkNamespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		setNetworkError(results, fmt.Sprintf("create iperf3 service: %s", err))
		finishStep(fmt.Sprintf("Could not expose iperf3 server on %s", server.Name))
		return
	}
	defer v.deleteBenchmarkService(serverName)
	if err := v.waitBenchmarkPodReady(ctx, serverName); err != nil {
		setNetworkError(results, fmt.Sprintf("start iperf3 server: %s", err))
		finishStep(fmt.Sprintf("Could not ready iperf3 server on %s", server.Name))
		return
	}
	finishStep(fmt.Sprintf("Started iperf3 server on %s", server.Name))

	results[0].NetworkBenchmark = &benchmarkNetworkResult{Image: benchmarkImage, Role: "server", PeerNode: "iperf3 clients"}
	for i := 1; i < len(nodes); i++ {
		node := nodes[i]
		result := &benchmarkNetworkResult{Image: benchmarkImage, Role: "client", PeerNode: server.Name}
		startStep(fmt.Sprintf("Running network throughput from %s to %s...", node.Name, server.Name))
		script := fmt.Sprintf(`set -eu
apk add --no-cache iperf3 >/dev/null
iperf3 -c %s -t 10 -J
`, serverName)
		logs, wallTime, err := v.runNodeBenchmarkJob(ctx, node, "network", script)
		result.WallTimeSeconds = wallTime
		if err != nil {
			result.Error = err.Error()
		}
		parseNetworkBenchmarkLogs(result, logs)
		if result.MbitsPerSecond == 0 && result.Error == "" {
			result.Error = "iperf3 did not report throughput"
		}
		results[i].NetworkBenchmark = result
		finishStep(fmt.Sprintf("Finished network throughput from %s to %s", node.Name, server.Name))
	}
}

func (v *BenchmarkView) runNodeBenchmarkJob(ctx context.Context, node corev1.Node, kind, script string) (string, float64, error) {
	jobName := benchmarkJobName(kind, node.Name)
	labels := benchmarkLabels(jobName)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: benchmarkNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(benchmarkJobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					NodeName:                      node.Name,
					TerminationGracePeriodSeconds: int64Ptr(benchmarkGracePeriodSeconds),
					Containers: []corev1.Container{{
						Name:            "benchmark",
						Image:           benchmarkImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", script},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "bench",
							MountPath: "/bench",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "bench",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}

	started := time.Now()
	if _, err := v.Cluster.BatchV1().Jobs(benchmarkNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", 0, fmt.Errorf("create job: %w", err)
	}
	defer v.deleteBenchmarkJob(jobName)

	var runErr error
	if err := v.waitBenchmarkJob(ctx, jobName); err != nil {
		runErr = err
	}
	wallTime := time.Since(started).Seconds()

	logs, err := v.benchmarkJobLogs(ctx, jobName)
	if err != nil && runErr == nil {
		runErr = fmt.Errorf("read logs: %w", err)
	}
	return logs, wallTime, runErr
}

func (v *BenchmarkView) waitBenchmarkJob(ctx context.Context, jobName string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := v.Cluster.BatchV1().Jobs(benchmarkNamespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				return nil
			}
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				message := strings.TrimSpace(condition.Message)
				if message == "" {
					message = strings.TrimSpace(condition.Reason)
				}
				if message == "" {
					message = "Benchmark job failed"
				}
				return fmt.Errorf("%s", message)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *BenchmarkView) waitBenchmarkPodReady(ctx context.Context, podName string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pod, err := v.Cluster.CoreV1().Pods(benchmarkNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("iperf3 server pod failed")
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (v *BenchmarkView) benchmarkJobLogs(ctx context.Context, jobName string) (string, error) {
	pods, err := v.Cluster.CoreV1().Pods(benchmarkNamespace).List(ctx, metav1.ListOptions{LabelSelector: "orchestrator.dev/benchmark=" + jobName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("benchmark pod was not found")
	}

	var lastErr error
	for _, pod := range pods.Items {
		req := v.Cluster.CoreV1().Pods(benchmarkNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "benchmark"})
		stream, err := req.Stream(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(data) > 0 {
			return string(data), nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

func (v *BenchmarkView) deleteBenchmarkJob(jobName string) {
	cleanupCtx, cancel := v.benchmarkCleanupContext()
	defer cancel()
	policy := metav1.DeletePropagationBackground
	err := v.Cluster.BatchV1().Jobs(benchmarkNamespace).Delete(cleanupCtx, jobName, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return
	}
}

func (v *BenchmarkView) deleteBenchmarkPod(podName string) {
	cleanupCtx, cancel := v.benchmarkCleanupContext()
	defer cancel()
	err := v.Cluster.CoreV1().Pods(benchmarkNamespace).Delete(cleanupCtx, podName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return
	}
}

func (v *BenchmarkView) deleteBenchmarkService(serviceName string) {
	cleanupCtx, cancel := v.benchmarkCleanupContext()
	defer cancel()
	err := v.Cluster.CoreV1().Services(benchmarkNamespace).Delete(cleanupCtx, serviceName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return
	}
}

func (v *BenchmarkView) benchmarkCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(v.ctx), 30*time.Second)
}

func benchmarkLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "orchestrator",
		"app.kubernetes.io/component":  "benchmark",
		"app.kubernetes.io/managed-by": "orchestrator",
		"orchestrator.dev/benchmark":   name,
	}
}

func benchmarkStepTotal(options benchmarkOptions, nodeCount int) int {
	total := 0
	if options.APILatency {
		total++
	}
	if options.ResourceScan {
		total++
	}
	if options.PodDensity {
		total++
	}
	perNode := 0
	if options.CPUPerformance {
		perNode++
	}
	if options.MemorySpeed {
		perNode++
	}
	if options.DiskSpeed {
		perNode++
	}
	total += perNode * nodeCount
	if options.NetworkSpeed {
		if nodeCount < 2 {
			total++
		} else {
			total += nodeCount
		}
	}
	return max(total, 1)
}

func (v *BenchmarkView) loadBenchmarkHistory() ([]benchmarkResult, error) {
	dir, err := benchmarkHistoryDir(v.ClusterPreferences.Value().Name)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var history []benchmarkResult
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var result benchmarkResult
		if err := json.Unmarshal(data, &result); err != nil {
			continue
		}
		history = append(history, result)
	}
	sort.Slice(history, func(i, j int) bool { return history[i].FinishedAt.After(history[j].FinishedAt) })
	return history, nil
}

func (v *BenchmarkView) saveBenchmarkHistory(result *benchmarkResult) error {
	dir, err := benchmarkHistoryDir(v.ClusterPreferences.Value().Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.json", result.FinishedAt.Format("20060102-150405"), benchmarkSafeName(result.Target))
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func benchmarkHistoryDir(cluster string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "orchestrator", "benchmarks", benchmarkSafeName(cluster)), nil
}

func benchmarkSafeName(value string) string {
	name := strings.ToLower(value)
	name = benchmarkNameRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		return "default"
	}
	return name
}

func setNetworkError(results []benchmarkNodeResult, message string) {
	for i := range results {
		results[i].NetworkBenchmark = &benchmarkNetworkResult{Image: benchmarkImage, Error: message}
	}
}

func parseCPUBenchmarkLogs(result *benchmarkCPUResult, logs string) {
	if match := benchmarkThreadsRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.Atoi(match[1]); err == nil {
			result.Threads = value
		}
	}
	if match := benchmarkEventsRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			result.EventsPerSecond = value
		}
	}
	if match := benchmarkTotalTimeRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			result.TotalTimeSeconds = value
		}
	}
}

func parseMemoryBenchmarkLogs(result *benchmarkMemoryResult, logs string) {
	if match := benchmarkThreadsRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.Atoi(match[1]); err == nil {
			result.Threads = value
		}
	}
	if match := benchmarkMemorySpeedRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			result.MiBPerSecond = value
		}
	}
	if match := benchmarkTotalTimeRe.FindStringSubmatch(logs); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			result.TotalTimeSeconds = value
		}
	}
}

func parseDiskBenchmarkLogs(result *benchmarkDiskResult, logs string) {
	if readJSON := extractBetween(logs, "ORCHESTRATOR_FIO_SEQ_READ_BEGIN", "ORCHESTRATOR_FIO_SEQ_READ_END"); readJSON != "" {
		if direction, ok := parseFIODirection(readJSON, true); ok {
			result.SequentialReadMiBPerSec = float64(direction.BWBytes) / 1024 / 1024
		}
	}
	if writeJSON := extractBetween(logs, "ORCHESTRATOR_FIO_SEQ_WRITE_BEGIN", "ORCHESTRATOR_FIO_SEQ_WRITE_END"); writeJSON != "" {
		if direction, ok := parseFIODirection(writeJSON, false); ok {
			result.SequentialWriteMiBPerSec = float64(direction.BWBytes) / 1024 / 1024
		}
	}
	if readJSON := extractBetween(logs, "ORCHESTRATOR_FIO_READ_BEGIN", "ORCHESTRATOR_FIO_READ_END"); readJSON != "" {
		if direction, ok := parseFIODirection(readJSON, true); ok {
			result.RandomReadMiBPerSec = float64(direction.BWBytes) / 1024 / 1024
			result.RandomReadIOPS = direction.IOPS
			result.RandomReadLatencyUS = direction.LatNS.Mean / 1000
		}
	}
	if writeJSON := extractBetween(logs, "ORCHESTRATOR_FIO_WRITE_BEGIN", "ORCHESTRATOR_FIO_WRITE_END"); writeJSON != "" {
		if direction, ok := parseFIODirection(writeJSON, false); ok {
			result.RandomWriteMiBPerSec = float64(direction.BWBytes) / 1024 / 1024
			result.RandomWriteIOPS = direction.IOPS
			result.RandomWriteLatencyUS = direction.LatNS.Mean / 1000
		}
	}
}

func parseNetworkBenchmarkLogs(result *benchmarkNetworkResult, logs string) {
	data := extractJSONDocument(logs)
	if data == "" {
		data = strings.TrimSpace(logs)
	}
	var parsed iperfJSONResult
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		if result.Error == "" {
			result.Error = fmt.Sprintf("parse iperf3 JSON: %s", err)
		}
		return
	}
	bitsPerSecond := parsed.End.SumReceived.BitsPerSecond
	if bitsPerSecond == 0 {
		bitsPerSecond = parsed.End.SumSent.BitsPerSecond
	}
	result.MbitsPerSecond = bitsPerSecond / 1000 / 1000
	result.Retransmits = parsed.End.SumSent.Retransmits
}

func parseFIODirection(data string, read bool) (fioJSONDirection, bool) {
	var parsed fioJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &parsed); err != nil || len(parsed.Jobs) == 0 {
		return fioJSONDirection{}, false
	}
	if read {
		return parsed.Jobs[0].Read, true
	}
	return parsed.Jobs[0].Write, true
}

func extractBetween(text, startMarker, endMarker string) string {
	start := strings.Index(text, startMarker)
	if start < 0 {
		return ""
	}
	start += len(startMarker)
	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		return ""
	}
	return text[start : start+end]
}

func extractJSONDocument(text string) string {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return ""
	}
	return text[start : end+1]
}

func benchmarkJobName(kind, nodeName string) string {
	base := strings.ToLower(nodeName)
	base = benchmarkNameRe.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "node"
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	maxBase := 63 - len("orchestrator---") - len(kind) - len(suffix)
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	return fmt.Sprintf("orchestrator-%s-%s-%s", kind, base, suffix)
}

func (v *BenchmarkView) clearResultGroups() {
	for _, g := range v.resultGroups {
		v.page.Remove(g)
	}
	v.resultGroups = nil
}

func (v *BenchmarkView) addResultGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.resultGroups = append(v.resultGroups, g)
}

func (v *BenchmarkView) renderResult(result *benchmarkResult) {
	v.clearResultGroups()
	maxCPUScore := 0.0
	maxMemoryScore := 0.0
	maxDiskReadScore := 0.0
	maxDiskWriteScore := 0.0
	maxNetworkScore := 0.0
	for _, node := range result.Nodes {
		if node.CPUBenchmark != nil && node.CPUBenchmark.Error == "" {
			maxCPUScore = max(maxCPUScore, node.CPUBenchmark.EventsPerSecond)
		}
		if node.MemoryBenchmark != nil && node.MemoryBenchmark.Error == "" {
			maxMemoryScore = max(maxMemoryScore, node.MemoryBenchmark.MiBPerSecond)
		}
		if node.DiskBenchmark != nil && node.DiskBenchmark.Error == "" {
			maxDiskReadScore = max(maxDiskReadScore, node.DiskBenchmark.SequentialReadMiBPerSec)
			maxDiskReadScore = max(maxDiskReadScore, node.DiskBenchmark.RandomReadMiBPerSec)
			maxDiskWriteScore = max(maxDiskWriteScore, node.DiskBenchmark.SequentialWriteMiBPerSec)
			maxDiskWriteScore = max(maxDiskWriteScore, node.DiskBenchmark.RandomWriteMiBPerSec)
		}
		if node.NetworkBenchmark != nil && node.NetworkBenchmark.Error == "" {
			maxNetworkScore = max(maxNetworkScore, node.NetworkBenchmark.MbitsPerSecond)
		}
	}

	// Run summary tiles
	summaryGroup := toolGroup("Run Summary",
		fmt.Sprintf("%s · finished %s", result.Target, result.FinishedAt.Format("Jan 2 15:04:05")))
	duration := result.FinishedAt.Sub(result.StartedAt).Round(time.Second)
	readyNodes := 0
	for _, n := range result.Nodes {
		if n.Ready {
			readyNodes++
		}
	}
	readyStyle, readyText := healthBadge(readyNodes, len(result.Nodes))
	tiles := []statTile{
		{Value: readyText, Caption: "Nodes ready", Style: readyStyle},
		{Value: duration.String(), Caption: "Duration", Style: "accent"},
	}
	if result.APILatency != nil {
		tiles = append(tiles, statTile{
			Value:   fmt.Sprintf("%.0f ms", result.APILatency.AvgMS),
			Caption: "API avg latency",
			Style:   "accent",
		})
	}
	statTilesGroup(summaryGroup, tiles)
	v.addResultGroup(summaryGroup)

	if result.APILatency != nil {
		apiGroup := toolGroup("Cluster API Latency",
			fmt.Sprintf("Measured over %d sample(s).", result.APILatency.Samples))
		apiMax := max(result.APILatency.MaxMS, 1)
		apiGroup.Add(progressRow("Average", fmt.Sprintf("%.1f ms", result.APILatency.AvgMS), result.APILatency.AvgMS, apiMax))
		apiGroup.Add(progressRow("Fastest", fmt.Sprintf("%.1f ms", result.APILatency.MinMS), result.APILatency.MinMS, apiMax))
		apiGroup.Add(progressRow("Slowest", fmt.Sprintf("%.1f ms", result.APILatency.MaxMS), result.APILatency.MaxMS, apiMax))
		if result.APILatency.Errors > 0 {
			apiGroup.Add(metricRow("Errors", "Failed list calls during sampling", fmt.Sprintf("%d", result.APILatency.Errors)))
		}
		v.addResultGroup(apiGroup)
	}

	for _, node := range result.Nodes {
		readyText := "Not ready"
		if node.Ready {
			readyText = "Ready"
		}
		nodeGroup := toolGroup(node.Name, readyText)

		if node.CPUBenchmark != nil {
			if node.CPUBenchmark.Error != "" {
				nodeGroup.Add(metricRow("CPU score", node.CPUBenchmark.Error, ""))
			} else {
				subtitle := "sysbench CPU events"
				if node.CPUBenchmark.Threads > 0 {
					subtitle = fmt.Sprintf("sysbench · %d threads", node.CPUBenchmark.Threads)
				}
				nodeGroup.Add(progressRow("CPU score",
					fmt.Sprintf("%.0f events/s", node.CPUBenchmark.EventsPerSecond),
					node.CPUBenchmark.EventsPerSecond, max(maxCPUScore, 1)))
				nodeGroup.Add(metricRow("CPU details", subtitle,
					fmt.Sprintf("%.1fs", node.CPUBenchmark.WallTimeSeconds)))
			}
		}
		if node.MemoryBenchmark != nil {
			if node.MemoryBenchmark.Error != "" {
				nodeGroup.Add(metricRow("Memory bandwidth", node.MemoryBenchmark.Error, ""))
			} else {
				nodeGroup.Add(progressRow("Memory bandwidth",
					fmt.Sprintf("%.0f MiB/s", node.MemoryBenchmark.MiBPerSecond),
					node.MemoryBenchmark.MiBPerSecond, max(maxMemoryScore, 1)))
			}
		}
		if node.DiskBenchmark != nil {
			if node.DiskBenchmark.Error != "" {
				nodeGroup.Add(metricRow("Disk I/O", node.DiskBenchmark.Error, ""))
			} else {
				nodeGroup.Add(progressRow("Sequential read",
					fmt.Sprintf("%.1f MiB/s", node.DiskBenchmark.SequentialReadMiBPerSec),
					node.DiskBenchmark.SequentialReadMiBPerSec, max(maxDiskReadScore, 1)))
				nodeGroup.Add(progressRow("Sequential write",
					fmt.Sprintf("%.1f MiB/s", node.DiskBenchmark.SequentialWriteMiBPerSec),
					node.DiskBenchmark.SequentialWriteMiBPerSec, max(maxDiskWriteScore, 1)))
				nodeGroup.Add(progressRow("4K random read",
					fmt.Sprintf("%.0f IOPS · %.0f µs", node.DiskBenchmark.RandomReadIOPS, node.DiskBenchmark.RandomReadLatencyUS),
					node.DiskBenchmark.RandomReadMiBPerSec, max(maxDiskReadScore, 1)))
				nodeGroup.Add(progressRow("4K random write",
					fmt.Sprintf("%.0f IOPS · %.0f µs", node.DiskBenchmark.RandomWriteIOPS, node.DiskBenchmark.RandomWriteLatencyUS),
					node.DiskBenchmark.RandomWriteMiBPerSec, max(maxDiskWriteScore, 1)))
			}
		}
		if node.NetworkBenchmark != nil {
			switch {
			case node.NetworkBenchmark.Error != "":
				nodeGroup.Add(metricRow("Network throughput", node.NetworkBenchmark.Error, ""))
			case node.NetworkBenchmark.Role == "server":
				nodeGroup.Add(metricRow("Network throughput", "iperf3 server for selected nodes", "—"))
			default:
				subtitle := fmt.Sprintf("to %s", node.NetworkBenchmark.PeerNode)
				if node.NetworkBenchmark.Retransmits > 0 {
					subtitle = fmt.Sprintf("%s · %d retransmits", subtitle, node.NetworkBenchmark.Retransmits)
				}
				nodeGroup.Add(progressRow("Network throughput",
					fmt.Sprintf("%.1f Mbit/s", node.NetworkBenchmark.MbitsPerSecond),
					node.NetworkBenchmark.MbitsPerSecond, max(maxNetworkScore, 1)))
				nodeGroup.Add(metricRow("Peer", subtitle, node.NetworkBenchmark.PeerNode))
			}
		}
		if result.Options.ResourceScan {
			if node.CPUUsageMilli != nil && node.CPUAllocatableMilli > 0 {
				nodeGroup.Add(progressRow("CPU usage",
					fmt.Sprintf("%dm / %dm", *node.CPUUsageMilli, node.CPUAllocatableMilli),
					float64(*node.CPUUsageMilli), float64(node.CPUAllocatableMilli)))
			} else {
				nodeGroup.Add(metricRow("CPU usage", "metrics-server unavailable", "—"))
			}
			nodeGroup.Add(progressRow("CPU allocatable",
				fmt.Sprintf("%dm / %dm", node.CPUAllocatableMilli, node.CPUCapacityMilli),
				float64(node.CPUAllocatableMilli), float64(node.CPUCapacityMilli)))
			if node.MemoryUsageBytes != nil && node.MemoryAllocatableBytes > 0 {
				nodeGroup.Add(progressRow("Memory usage",
					fmt.Sprintf("%s / %s", formatBytes(*node.MemoryUsageBytes), formatBytes(node.MemoryAllocatableBytes)),
					float64(*node.MemoryUsageBytes), float64(node.MemoryAllocatableBytes)))
			} else {
				nodeGroup.Add(metricRow("Memory usage", "metrics-server unavailable", "—"))
			}
			nodeGroup.Add(progressRow("Memory allocatable",
				fmt.Sprintf("%s / %s", formatBytes(node.MemoryAllocatableBytes), formatBytes(node.MemoryCapacityBytes)),
				float64(node.MemoryAllocatableBytes), float64(node.MemoryCapacityBytes)))
		}
		if node.PodCount != nil && node.PodCapacity > 0 {
			nodeGroup.Add(progressRow("Pod density",
				fmt.Sprintf("%d / %d pods", *node.PodCount, node.PodCapacity),
				float64(*node.PodCount), float64(node.PodCapacity)))
		}
		if result.Options.ResourceScan {
			if node.PowerDrawWatts != nil {
				nodeGroup.Add(metricRow("Power draw", "Reported by metrics-server", fmt.Sprintf("%.1f W", *node.PowerDrawWatts)))
			} else {
				nodeGroup.Add(metricRow("Power draw", node.PowerSource, "—"))
			}
		}
		v.addResultGroup(nodeGroup)
	}
}

func (v *BenchmarkView) renderBenchmarkHistory(current *benchmarkResult, history []benchmarkResult) {
	if len(history) == 0 {
		historyGroup := toolGroup("Benchmark History", "Previous runs are saved per cluster.")
		emptyStatusGroup(historyGroup, "No previous runs",
			"This is the first benchmark saved for this cluster.",
			"emblem-default-symbolic")
		v.addResultGroup(historyGroup)
		return
	}

	if previous := firstComparableBenchmark(current, history); previous != nil {
		compareGroup := toolGroup("Compared With Previous Run",
			"Previous run "+previous.FinishedAt.Format("Jan 2 15:04:05"))
		appendBenchmarkComparisons(compareGroup, current, previous)
		v.addResultGroup(compareGroup)
	}

	historyGroup := toolGroup("Benchmark History",
		fmt.Sprintf("%d previous run(s) saved for this cluster.", len(history)))
	limit := min(len(history), 5)
	for i := 0; i < limit; i++ {
		run := history[i]
		historyGroup.Add(metricRow(
			run.FinishedAt.Format("Jan 2 15:04:05"),
			fmt.Sprintf("%s · %d nodes", run.Target, len(run.Nodes)),
			run.FinishedAt.Format("15:04:05"),
		))
	}
	v.addResultGroup(historyGroup)
}

func firstComparableBenchmark(current *benchmarkResult, history []benchmarkResult) *benchmarkResult {
	for i := range history {
		if history[i].Cluster == current.Cluster && history[i].Target == current.Target && history[i].FinishedAt.Before(current.FinishedAt) {
			return &history[i]
		}
	}
	return nil
}

func appendBenchmarkComparisons(group *adw.PreferencesGroup, current, previous *benchmarkResult) {
	previousNodes := map[string]benchmarkNodeResult{}
	for _, node := range previous.Nodes {
		previousNodes[node.Name] = node
	}
	rows := 0
	for _, node := range current.Nodes {
		previousNode, ok := previousNodes[node.Name]
		if !ok {
			continue
		}
		if node.CPUBenchmark != nil && previousNode.CPUBenchmark != nil && node.CPUBenchmark.Error == "" && previousNode.CPUBenchmark.Error == "" {
			delta, value := benchmarkDelta(node.CPUBenchmark.EventsPerSecond, previousNode.CPUBenchmark.EventsPerSecond, "events/s")
			group.Add(metricRow(node.Name+" · CPU", delta, value))
			rows++
		}
		if node.MemoryBenchmark != nil && previousNode.MemoryBenchmark != nil && node.MemoryBenchmark.Error == "" && previousNode.MemoryBenchmark.Error == "" {
			delta, value := benchmarkDelta(node.MemoryBenchmark.MiBPerSecond, previousNode.MemoryBenchmark.MiBPerSecond, "MiB/s")
			group.Add(metricRow(node.Name+" · Memory", delta, value))
			rows++
		}
		if node.NetworkBenchmark != nil && previousNode.NetworkBenchmark != nil && node.NetworkBenchmark.Error == "" && previousNode.NetworkBenchmark.Error == "" && node.NetworkBenchmark.Role != "server" {
			delta, value := benchmarkDelta(node.NetworkBenchmark.MbitsPerSecond, previousNode.NetworkBenchmark.MbitsPerSecond, "Mbit/s")
			group.Add(metricRow(node.Name+" · Network", delta, value))
			rows++
		}
	}
	if rows == 0 {
		emptyStatusGroup(group, "Nothing to compare",
			"No matching benchmark metrics were available between the runs.",
			"emblem-default-symbolic")
	}
}

func benchmarkDelta(current, previous float64, unit string) (subtitle, value string) {
	value = fmt.Sprintf("%.1f %s", current, unit)
	if previous == 0 {
		return "no previous value", value
	}
	change := ((current - previous) / previous) * 100
	return fmt.Sprintf("%.1f %s previously · %+.1f%%", previous, unit, change), value
}

func benchmarkCard() *gtk.Box {
	card := gtk.NewBox(gtk.OrientationVertical, 8)
	card.AddCSSClass("benchmark-card")
	return card
}

func int32Ptr(value int32) *int32 { return &value }

func int64Ptr(value int64) *int64 { return &value }

func (v *BenchmarkView) save() {
	if v.last == nil {
		return
	}
	chooser := gtk.NewFileChooserNative("Save benchmark results", ctxt.MustFrom[*gtk.Window](v.ctx), gtk.FileChooserActionSave, "Save", "Cancel")
	chooser.SetCurrentName(fmt.Sprintf("orchestrator-benchmark-%s.json", time.Now().Format("20060102-150405")))
	chooser.ConnectResponse(func(responseID int) {
		if responseID != int(gtk.ResponseAccept) || chooser.File() == nil {
			return
		}
		data, err := json.MarshalIndent(v.last, "", "  ")
		if err != nil {
			widget.ShowErrorDialog(v.ctx, "Could not encode benchmark results", err)
			return
		}
		if err := os.WriteFile(chooser.File().Path(), data, 0o600); err != nil {
			widget.ShowErrorDialog(v.ctx, "Could not save benchmark results", err)
		}
	})
	chooser.Show()
}

func nodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func sectionLabel(title string) *gtk.Label {
	label := gtk.NewLabel(title)
	label.SetHAlign(gtk.AlignStart)
	label.AddCSSClass("heading")
	return label
}

func textRow(title, value string) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	row.SetSubtitle(value)
	return row
}

func barRow(title, text string, value, total float64) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	row.SetSubtitle(text)
	bar := gtk.NewProgressBar()
	bar.SetSizeRequest(180, -1)
	bar.SetShowText(true)
	bar.SetText(text)
	if total > 0 {
		bar.SetFraction(min(max(value/total, 0), 1))
	}
	row.AddSuffix(bar)
	return row
}

func clearBox(box *gtk.Box) {
	for child := box.FirstChild(); child != nil; child = box.FirstChild() {
		box.Remove(child)
	}
}

func formatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
