APP_ID = dev.silkepilon.Orchestrator
FLATPAK_BUILDER_FLAGS ?= --disable-rofiles-fuse
FLATPAK_MANIFEST = $(APP_ID).yml
SEMVER = go run github.com/maykonlsf/semver-cli/cmd/semver@latest

.PHONY: patch
patch:
	@if [ "$$(git rev-parse --abbrev-ref HEAD)" != "main" ]; then exit 1; fi
	git pull -r
	$(SEMVER) up release

.PHONY: minor
minor:
	@if [ "$$(git rev-parse --abbrev-ref HEAD)" != "main" ]; then exit 1; fi
	git pull -r
	$(SEMVER) up minor

.PHONY: release
major:
	@if [ "$$(git rev-parse --abbrev-ref HEAD)" != "main" ]; then exit 1; fi
	git pull -r
	$(SEMVER) up major

.PHONY: release
release:
	sed -i "/<releases>/a \    <release version=\"$$($(SEMVER) get release)\" date=\"$$(date +%F)\">\n      <url>https://github.com/SilkePilon/Orchestrator/releases/tag/$$($(SEMVER) get release)</url>\n    </release>" dev.silkepilon.Orchestrator.metainfo.xml
	git add .semver.yaml dev.silkepilon.Orchestrator.metainfo.xml
	git commit -m "$$($(SEMVER) get release)"
	git tag -a -m "$$($(SEMVER) get release)" "$$($(SEMVER) get release)"

.PHONY: build
build:
	GOMEMLIMIT=2GiB go build -p 1 ./...

.PHONY: flatpak-sources
flatpak-sources:
	python3 hack/gen-flatpak-go-sources.py go.sum flatpak-go-sources.json

.PHONY: flatpak-validate
flatpak-validate:
	desktop-file-validate $(APP_ID).desktop
	appstreamcli validate --pedantic $(APP_ID).metainfo.xml

.PHONY: flatpak-build
flatpak-build: flatpak-validate
	flatpak-builder $(FLATPAK_BUILDER_FLAGS) --force-clean build-flatpak $(FLATPAK_MANIFEST)

.PHONY: flatpak-install
flatpak-install: flatpak-validate
	flatpak-builder $(FLATPAK_BUILDER_FLAGS) --user --install --force-clean build-flatpak $(FLATPAK_MANIFEST)

.PHONY: flatpak-run
flatpak-run:
	flatpak run $(APP_ID)