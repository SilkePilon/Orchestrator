package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/getseabird/seabird/internal/ui/common"
	"github.com/getseabird/seabird/widget"
	corev1 "k8s.io/api/core/v1"
)

type SecurityScanView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	refresh *gtk.Button
	status  *gtk.Label
	results *gtk.Box
}

type securityOverview struct {
	Pods             int
	Containers       int
	Services         int
	CriticalFindings int
	HighFindings     int
	MediumFindings   int
	Findings         []securityFinding
}

type securityFinding struct {
	Title    string
	Detail   string
	Severity int
}

func NewSecurityScanView(ctx context.Context, state *common.ClusterState) *SecurityScanView {
	view := &SecurityScanView{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Security", state.ClusterPreferences.Value().Name))

	view.refresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	view.refresh.AddCSSClass("flat")
	view.refresh.SetTooltipText("Refresh security scan")
	view.refresh.ConnectClicked(view.refreshSecurity)
	header.PackEnd(view.refresh)
	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)

	group := adw.NewPreferencesGroup()
	group.SetTitle("Security / Best-Practice Scan")
	group.SetDescription("Local checks for risky pod settings, missing probes, broad exposure, and weak image hygiene.")
	page.Add(group)

	view.status = gtk.NewLabel("Scanning cluster...")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.AddCSSClass("dim-label")
	group.Add(view.status)

	view.results = gtk.NewBox(gtk.OrientationVertical, 12)
	group.Add(view.results)

	view.refreshSecurity()
	return view
}

func (v *SecurityScanView) refreshSecurity() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Scanning cluster...")
	clearBox(v.results)

	go func() {
		overview, err := v.collectSecurity()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Security scan failed")
				widget.ShowErrorDialog(v.ctx, "Could not run security scan", err)
				return
			}
			v.status.SetText("Security scan complete")
			v.renderSecurity(overview)
		})
	}()
}

func (v *SecurityScanView) collectSecurity() (*securityOverview, error) {
	overview := &securityOverview{}
	var pods corev1.PodList
	if err := v.List(v.ctx, &pods); err != nil {
		return nil, err
	}
	overview.Pods = len(pods.Items)
	for _, pod := range pods.Items {
		scanPodSecurity(&pod, overview)
	}

	var services corev1.ServiceList
	if err := v.List(v.ctx, &services); err != nil {
		return nil, err
	}
	overview.Services = len(services.Items)
	for _, service := range services.Items {
		scanServiceSecurity(&service, overview)
	}

	sort.Slice(overview.Findings, func(i, j int) bool {
		if overview.Findings[i].Severity == overview.Findings[j].Severity {
			return overview.Findings[i].Title < overview.Findings[j].Title
		}
		return overview.Findings[i].Severity > overview.Findings[j].Severity
	})
	for _, finding := range overview.Findings {
		switch {
		case finding.Severity >= 90:
			overview.CriticalFindings++
		case finding.Severity >= 70:
			overview.HighFindings++
		case finding.Severity >= 40:
			overview.MediumFindings++
		}
	}
	return overview, nil
}

func scanPodSecurity(pod *corev1.Pod, overview *securityOverview) {
	podName := pod.Namespace + "/" + pod.Name
	if pod.Spec.HostNetwork {
		addSecurityFinding(overview, podName, "Uses hostNetwork.", 85)
	}
	if pod.Spec.HostPID {
		addSecurityFinding(overview, podName, "Uses hostPID.", 90)
	}
	if pod.Spec.HostIPC {
		addSecurityFinding(overview, podName, "Uses hostIPC.", 80)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			addSecurityFinding(overview, podName, "Mounts hostPath volume "+volume.Name+".", 80)
		}
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		addSecurityFinding(overview, podName, "Pod does not require runAsNonRoot.", 45)
	}

	for _, container := range pod.Spec.Containers {
		overview.Containers++
		scanContainerSecurity(podName, container, overview)
	}
}

func scanContainerSecurity(podName string, container corev1.Container, overview *securityOverview) {
	name := podName + " · " + container.Name
	if imageUsesLatest(container.Image) {
		addSecurityFinding(overview, name, "Image uses latest or has no explicit tag.", 55)
	}
	if container.SecurityContext == nil {
		addSecurityFinding(overview, name, "Container has no securityContext.", 45)
	} else {
		if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
			addSecurityFinding(overview, name, "Privileged container.", 95)
		}
		if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
			addSecurityFinding(overview, name, "Privilege escalation is not disabled.", 75)
		}
		if container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
			addSecurityFinding(overview, name, "Container does not require runAsNonRoot.", 45)
		}
		if caps := container.SecurityContext.Capabilities; caps != nil && len(caps.Add) > 0 {
			addSecurityFinding(overview, name, "Adds Linux capabilities: "+capabilityList(caps.Add), 70)
		}
	}
	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() {
		addSecurityFinding(overview, name, "Missing CPU or memory request.", 40)
	}
	if container.ReadinessProbe == nil {
		addSecurityFinding(overview, name, "Missing readiness probe.", 35)
	}
	if container.LivenessProbe == nil {
		addSecurityFinding(overview, name, "Missing liveness probe.", 35)
	}
}

func scanServiceSecurity(service *corev1.Service, overview *securityOverview) {
	name := service.Namespace + "/" + service.Name
	switch service.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		addSecurityFinding(overview, name, "LoadBalancer service exposes workloads outside the cluster.", 65)
	case corev1.ServiceTypeNodePort:
		addSecurityFinding(overview, name, "NodePort service exposes node-level ports.", 55)
	}
}

func addSecurityFinding(overview *securityOverview, title, detail string, severity int) {
	overview.Findings = append(overview.Findings, securityFinding{Title: title, Detail: detail, Severity: severity})
}

func (v *SecurityScanView) renderSecurity(overview *securityOverview) {
	clearBox(v.results)

	summary := benchmarkCard()
	summary.Append(sectionLabel("Summary"))
	summary.Append(textRow("Pods", fmt.Sprintf("%d", overview.Pods)))
	summary.Append(textRow("Containers", fmt.Sprintf("%d", overview.Containers)))
	summary.Append(textRow("Services", fmt.Sprintf("%d", overview.Services)))
	summary.Append(textRow("Critical findings", fmt.Sprintf("%d", overview.CriticalFindings)))
	summary.Append(textRow("High findings", fmt.Sprintf("%d", overview.HighFindings)))
	summary.Append(textRow("Medium findings", fmt.Sprintf("%d", overview.MediumFindings)))
	v.results.Append(summary)

	findings := benchmarkCard()
	findings.Append(sectionLabel("Findings"))
	if len(overview.Findings) == 0 {
		findings.Append(textRow("No findings", "No obvious security or best-practice issues were found."))
	} else {
		limit := min(len(overview.Findings), 100)
		for i := 0; i < limit; i++ {
			finding := overview.Findings[i]
			findings.Append(textRow(severityLabel(finding.Severity)+" · "+finding.Title, finding.Detail))
		}
		if len(overview.Findings) > limit {
			findings.Append(textRow("More", fmt.Sprintf("%d additional findings", len(overview.Findings)-limit)))
		}
	}
	v.results.Append(findings)
}

func imageUsesLatest(image string) bool {
	if strings.HasSuffix(image, ":latest") {
		return true
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	return lastColon < lastSlash
}

func capabilityList(caps []corev1.Capability) string {
	values := make([]string, 0, len(caps))
	for _, cap := range caps {
		values = append(values, string(cap))
	}
	return strings.Join(values, ", ")
}

func severityLabel(severity int) string {
	switch {
	case severity >= 90:
		return "Critical"
	case severity >= 70:
		return "High"
	case severity >= 40:
		return "Medium"
	default:
		return "Low"
	}
}
