# Seabird

Seabird is a Kubernetes IDE designed for the GNOME desktop. Explore and manage
your clusters with a simple and intuitive interface. Equipped with essential
features such as a terminal for executing commands, monitoring through logs and
metrics, and a resource editor that conveniently places the API reference at
your fingertips.

![Screenshot](https://getseabird.github.io/images/screenshot.png)

## Download

Downloads for all platforms are available under
[releases](https://github.com/getseabird/seabird/releases). On Linux, we
recommend using the Flatpak package.

<a href='https://flathub.org/apps/dev.skynomads.Seabird'>
  <img width='140' alt='Download on Flathub' src='https://flathub.org/api/badge?locale=en'/>
</a>

## Features

### Cluster Management

- **Auto-discovery** — automatically detects kubeconfig files at `~/.kube/config` and any paths in `$KUBECONFIG`
- **Multiple clusters** — connect to and switch between multiple clusters from the welcome screen
- **Manual configuration** — configure clusters by host URL, bearer token, TLS certificates, or exec-based auth
- **Read-only mode** — optionally prevent any write operations against a cluster
- **Keyboard shortcuts** — `Ctrl+N` opens a new window, `Ctrl+Q` disconnects from the current cluster

### Resource Browser

- **Full API coverage** — browse every resource type the cluster API exposes, including CRDs
- **Favourites** — pin frequently-used resource types to the sidebar for quick access (defaults: Pods, ConfigMaps, Secrets, PVCs, Deployments, StatefulSets, Services, Ingresses, Namespaces, Nodes)
- **Pinned objects** — pin individual objects to the sidebar to keep them a click away
- **Namespace filter** — scope the resource list to one or all namespaces
- **Search** — filter resources by name in real time

### Resource Details

Clicking any resource opens a detail panel with rich, resource-specific properties:

| Resource | Extra detail shown |
|---|---|
| **Pod** | Per-container state, restart count, image, command, env vars (resolved from ConfigMaps/Secrets), ports, CPU & memory usage vs. requests/limits |
| **Deployment / ReplicaSet / StatefulSet** | Linked pod list with status icons; StatefulSets also show volume claim templates |
| **Node** | Architecture, container runtime, kernel, kubelet version, OS image, allocatable CPU & memory, pod list |
| **Service** | Cluster IP, port list |
| **ConfigMap / Secret** | Key–value data |
| **PersistentVolumeClaim** | Storage class, capacity request, access modes, linked PV |
| **PersistentVolume** | Storage class, capacity, access modes, reclaim policy, phase, linked claim |

### Metrics

- CPU and memory usage bars are shown inline in the **Pods** and **Nodes** list columns (requires metrics-server)
- Per-container CPU and memory usage with requests and limits are shown in the pod detail panel

### Logs

- Stream live container logs directly from the resource detail panel
- Each container has a dedicated **Logs** navigation page

### Terminal (exec)

- Open an interactive shell (`/bin/sh`) inside any container from its detail panel
- Full VTE-based terminal emulator embedded in the UI (Linux only)

### Port Forwarding

- Forward any container port to `localhost` with a single click from the pod detail panel
- Active forwards show the local port; click again to stop

### Resource Editor

- Edit any resource as YAML with syntax highlighting (GtkSourceView 5)
- Inline Kubernetes API reference available while editing
- Create new resources directly from the editor

### Preferences

- **Color scheme** — choose Default, Light, or Dark
- **Cluster settings** — add, remove, or reconfigure clusters at any time
- Preferences are saved automatically to `$XDG_CONFIG_HOME/seabird/prefs.json`

### Other

- Update notifications — a toast appears when a new release is available
- Crash/panic window catches unhandled errors and displays them gracefully

## Setup

### Prerequisites

Seabird connects to Kubernetes clusters using standard kubeconfig files. Make
sure you have a valid kubeconfig at `~/.kube/config` or exported in
`$KUBECONFIG` before launching. Seabird will detect all contexts automatically.

To see CPU and memory metrics, your cluster must have
[metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed.

### First Launch

1. Launch Seabird. The welcome screen lists all clusters found in your kubeconfig.
2. Click a cluster to connect. A spinner indicates the connection attempt.
3. Once connected, the main window opens with the resource sidebar on the left.
4. Use the sidebar to navigate resource types. Click any row to open its detail panel.

### Adding a Cluster Manually

1. Open **Preferences** (hamburger menu → Preferences, or the gear icon).
2. In the **Clusters** group, click the **+** button.
3. Fill in the cluster name, host URL, and authentication details (bearer token,
   TLS certificates, or an exec provider).
4. Save and return to the welcome screen to connect.

## Building From Source

### Dependencies

#### Fedora

```sh
sudo dnf install gtk4-devel gtksourceview5-devel libadwaita-devel gobject-introspection-devel glib2-devel vte291-gtk4-devel golang
```

#### Debian / Ubuntu

```sh
sudo apt install libgtk-4-dev libgtksourceview-5-dev libadwaita-1-dev libgirepository1.0-dev libglib2.0-dev-bin libvte-2.91-gtk4-dev golang-go
```

### Build

Generate the embedded icon resource file, then build:

```sh
go generate ./...
go build
```

Run directly without installing:

```sh
go run .
```

## Reporting Issues

If you experience problems, please open an
[issue](https://github.com/getseabird/seabird/issues). Try to include as much
information as possible, such as version, operating system and reproduction
steps.

For feature suggestions, please create a
[discussion](https://github.com/getseabird/seabird/discussions). If you have a
concrete vision for the feature, open an issue instead and use the proposal
template.

## License

Seabird is available under the terms of the Mozilla Public License v2, a copy of
the license is distributed in the LICENSE file.

Note: This is paid software with an unlimited free trial.
