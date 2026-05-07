package widget

import (
	"context"
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/leaanthony/go-ansi-parser"
	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/internal/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type LogPage struct {
	*adw.NavigationPage
}

func NewLogPage(ctx context.Context, cluster *api.Cluster, pod *corev1.Pod, container string) *LogPage {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("view")
	p := LogPage{NavigationPage: adw.NewNavigationPage(box, container)}

	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.AddCSSClass("flat")
	box.Append(header)

	buffer, view := newLogBufferView()

	scrolledWindow := gtk.NewScrolledWindow()
	scrolledWindow.SetChild(view)
	scrolledWindow.SetVExpand(true)
	box.Append(scrolledWindow)

	logs, err := podLogs(ctx, cluster, pod, container)
	if err != nil {
		ShowErrorDialog(ctx, "Could not load logs", err)
	} else {
		appendLogText(buffer, string(logs))
	}

	return &p
}

func NewAggregatedLogPage(ctx context.Context, cluster *api.Cluster, title, namespace string, selector *metav1.LabelSelector) *LogPage {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("view")
	p := LogPage{NavigationPage: adw.NewNavigationPage(box, title)}

	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.AddCSSClass("flat")
	box.Append(header)

	buffer, view := newLogBufferView()
	scrolledWindow := gtk.NewScrolledWindow()
	scrolledWindow.SetChild(view)
	scrolledWindow.SetVExpand(true)
	box.Append(scrolledWindow)

	logs, err := aggregateLogs(ctx, cluster, namespace, selector)
	if err != nil {
		ShowErrorDialog(ctx, "Could not load aggregated logs", err)
	} else {
		appendLogText(buffer, logs)
	}

	return &p
}

func newLogBufferView() (*gtksource.Buffer, *gtksource.View) {
	buffer := gtksource.NewBuffer(nil)
	util.SetSourceColorScheme(buffer)
	view := gtksource.NewViewWithBuffer(buffer)
	view.SetEditable(false)
	view.SetWrapMode(gtk.WrapWord)
	view.SetShowLineNumbers(true)
	view.SetMonospace(true)
	return buffer, view
}

func appendLogText(buffer *gtksource.Buffer, logs string) {
	text, err := ansi.Parse(logs)
	if err != nil {
		buffer.Insert(buffer.EndIter(), logs)
		return
	}
	for _, text := range text {
		var attr []string
		if text.FgCol != nil {
			attr = append(attr, fmt.Sprintf(`foreground="%s"`, text.FgCol.Hex))
		}
		if text.BgCol != nil {
			attr = append(attr, fmt.Sprintf(`background="%s"`, text.BgCol.Hex))
		}
		buffer.InsertMarkup(buffer.EndIter(), fmt.Sprintf(`<span %s>%s</span>`, strings.Join(attr, " "), html.EscapeString(text.Label)))
	}
}

func aggregateLogs(ctx context.Context, cluster *api.Cluster, namespace string, selector *metav1.LabelSelector) (string, error) {
	if selector == nil {
		return "", fmt.Errorf("workload does not define a pod selector")
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", err
	}
	if labelSelector == nil {
		labelSelector = labels.Everything()
	}

	var pods corev1.PodList
	if err := cluster.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "No matching pods found.\n", nil
	}

	var output strings.Builder
	tailLines := int64(500)
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			logs, err := podLogsWithOptions(ctx, cluster, &pod, container.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &tailLines})
			output.WriteString(fmt.Sprintf("\n[%s/%s] %s\n", pod.Name, container.Name, strings.Repeat("-", 48)))
			if err != nil {
				output.WriteString(fmt.Sprintf("error: %s\n", err))
				continue
			}
			if len(logs) == 0 {
				output.WriteString("No logs returned.\n")
				continue
			}
			output.Write(logs)
			if !strings.HasSuffix(string(logs), "\n") {
				output.WriteString("\n")
			}
		}
	}
	output.WriteString(fmt.Sprintf("\nLoaded at %s\n", time.Now().Format("15:04:05")))
	return output.String(), nil
}

func podLogs(ctx context.Context, cluster *api.Cluster, pod *corev1.Pod, container string) ([]byte, error) {
	return podLogsWithOptions(ctx, cluster, pod, container, &corev1.PodLogOptions{Container: container})
}

func podLogsWithOptions(ctx context.Context, cluster *api.Cluster, pod *corev1.Pod, container string, options *corev1.PodLogOptions) ([]byte, error) {
	if options == nil {
		options = &corev1.PodLogOptions{Container: container}
	}
	req := cluster.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, options)
	r, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}
