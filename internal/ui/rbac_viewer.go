package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	rbacv1 "k8s.io/api/rbac/v1"
)

type RBACViewer struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	refresh *gtk.Button
	status  *gtk.Label
	results *gtk.Box
}

type rbacOverview struct {
	Roles               []rbacv1.Role
	ClusterRoles        []rbacv1.ClusterRole
	RoleBindings        []rbacv1.RoleBinding
	ClusterRoleBindings []rbacv1.ClusterRoleBinding
}

func NewRBACViewer(ctx context.Context, state *common.ClusterState) *RBACViewer {
	view := &RBACViewer{
		ToolbarView:  adw.NewToolbarView(),
		ClusterState: state,
		ctx:          ctx,
	}
	view.AddCSSClass("view")
	view.SetTopBarStyle(adw.ToolbarRaised)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("RBAC", state.ClusterPreferences.Value().Name))

	view.refresh = gtk.NewButtonFromIconName("view-refresh-symbolic")
	view.refresh.AddCSSClass("flat")
	view.refresh.SetTooltipText("Refresh RBAC overview")
	view.refresh.ConnectClicked(view.refreshRBAC)
	header.PackEnd(view.refresh)
	view.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()
	scroll.SetChild(page)
	view.SetContent(scroll)

	group := adw.NewPreferencesGroup()
	group.SetTitle("RBAC Viewer")
	group.SetDescription("Roles, ClusterRoles, and their bindings across the cluster.")
	page.Add(group)

	view.status = gtk.NewLabel("Loading RBAC...")
	view.status.SetHAlign(gtk.AlignStart)
	view.status.AddCSSClass("dim-label")
	group.Add(view.status)

	view.results = gtk.NewBox(gtk.OrientationVertical, 12)
	group.Add(view.results)

	view.refreshRBAC()
	return view
}

func (v *RBACViewer) refreshRBAC() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing RBAC...")
	clearBox(v.results)

	go func() {
		overview, err := v.collectRBAC()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Could not load RBAC")
				widget.ShowErrorDialog(v.ctx, "Could not load RBAC", err)
				return
			}
			v.status.SetText("RBAC loaded")
			v.renderRBAC(overview)
		})
	}()
}

func (v *RBACViewer) collectRBAC() (*rbacOverview, error) {
	overview := &rbacOverview{}
	var roles rbacv1.RoleList
	if err := v.List(v.ctx, &roles); err != nil {
		return nil, err
	}
	overview.Roles = roles.Items

	var clusterRoles rbacv1.ClusterRoleList
	if err := v.List(v.ctx, &clusterRoles); err != nil {
		return nil, err
	}
	overview.ClusterRoles = clusterRoles.Items

	var bindings rbacv1.RoleBindingList
	if err := v.List(v.ctx, &bindings); err != nil {
		return nil, err
	}
	overview.RoleBindings = bindings.Items

	var clusterBindings rbacv1.ClusterRoleBindingList
	if err := v.List(v.ctx, &clusterBindings); err != nil {
		return nil, err
	}
	overview.ClusterRoleBindings = clusterBindings.Items
	return overview, nil
}

func (v *RBACViewer) renderRBAC(overview *rbacOverview) {
	clearBox(v.results)

	summary := benchmarkCard()
	summary.Append(sectionLabel("Summary"))
	summary.Append(textRow("Roles", fmt.Sprintf("%d", len(overview.Roles))))
	summary.Append(textRow("ClusterRoles", fmt.Sprintf("%d", len(overview.ClusterRoles))))
	summary.Append(textRow("RoleBindings", fmt.Sprintf("%d", len(overview.RoleBindings))))
	summary.Append(textRow("ClusterRoleBindings", fmt.Sprintf("%d", len(overview.ClusterRoleBindings))))
	v.results.Append(summary)

	roleCard := benchmarkCard()
	roleCard.Append(sectionLabel("Roles With Broad Access"))
	appendBroadRoleRows(roleCard, overview)
	v.results.Append(roleCard)

	bindingCard := benchmarkCard()
	bindingCard.Append(sectionLabel("Bindings"))
	appendBindingRows(bindingCard, overview)
	v.results.Append(bindingCard)
}

func appendBroadRoleRows(card *gtk.Box, overview *rbacOverview) {
	type roleFinding struct {
		Name string
		Text string
	}
	var findings []roleFinding
	for _, role := range overview.Roles {
		if text := broadRuleSummary(role.Rules); text != "" {
			findings = append(findings, roleFinding{Name: role.Namespace + "/" + role.Name, Text: text})
		}
	}
	for _, role := range overview.ClusterRoles {
		if text := broadRuleSummary(role.Rules); text != "" {
			findings = append(findings, roleFinding{Name: role.Name, Text: text})
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Name < findings[j].Name })
	if len(findings) == 0 {
		card.Append(textRow("Broad roles", "No wildcard admin-style rules found."))
		return
	}
	for i, finding := range findings {
		if i >= 40 {
			card.Append(textRow("More", fmt.Sprintf("%d additional broad roles", len(findings)-i)))
			return
		}
		card.Append(textRow(finding.Name, finding.Text))
	}
}

func appendBindingRows(card *gtk.Box, overview *rbacOverview) {
	type bindingRow struct {
		Name string
		Text string
	}
	var rows []bindingRow
	for _, binding := range overview.RoleBindings {
		rows = append(rows, bindingRow{
			Name: binding.Namespace + "/" + binding.Name,
			Text: fmt.Sprintf("%s -> %s (%s)", subjectSummary(binding.Subjects), binding.RoleRef.Name, binding.RoleRef.Kind),
		})
	}
	for _, binding := range overview.ClusterRoleBindings {
		rows = append(rows, bindingRow{
			Name: binding.Name,
			Text: fmt.Sprintf("%s -> %s (%s)", subjectSummary(binding.Subjects), binding.RoleRef.Name, binding.RoleRef.Kind),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if len(rows) == 0 {
		card.Append(textRow("Bindings", "No RBAC bindings found."))
		return
	}
	for i, row := range rows {
		if i >= 80 {
			card.Append(textRow("More", fmt.Sprintf("%d additional bindings", len(rows)-i)))
			return
		}
		card.Append(textRow(row.Name, row.Text))
	}
}

func broadRuleSummary(rules []rbacv1.PolicyRule) string {
	for _, rule := range rules {
		if containsString(rule.Verbs, "*") && (containsString(rule.Resources, "*") || containsString(rule.NonResourceURLs, "*")) {
			return "Wildcard verbs and resources"
		}
		if containsString(rule.Verbs, "*") {
			return "Wildcard verbs on " + strings.Join(rule.Resources, ", ")
		}
	}
	return ""
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func subjectSummary(subjects []rbacv1.Subject) string {
	if len(subjects) == 0 {
		return "No subjects"
	}
	parts := make([]string, 0, min(len(subjects), 3))
	for i, subject := range subjects {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(subjects)-i))
			break
		}
		name := subject.Name
		if subject.Namespace != "" {
			name = subject.Namespace + "/" + name
		}
		parts = append(parts, subject.Kind+":"+name)
	}
	return strings.Join(parts, ", ")
}
