#!/bin/zsh
# Replx Edge dev environment setup (macOS arm64, zerobrew).
# Installs Go + Docker CLI + colima/qemu, applies known zerobrew
# workarounds, starts the VM, and verifies the toolchain.
set -euo pipefail

echo "==> Go"
zb install go
go version

echo "==> Docker CLI + VM"
zb install docker colima qemu docker-compose

# Workaround 1: zerobrew qemu lacks the hypervisor entitlement.
if ! codesign -d --entitlements - /opt/zerobrew/bin/qemu-system-aarch64 2>/dev/null | grep -q com.apple.security.hypervisor; then
  echo "==> signing qemu with hypervisor entitlement"
  cat > /tmp/qemu-entitlements.xml <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.hypervisor</key>
    <true/>
</dict>
</plist>
EOF
  codesign --force --sign - --entitlements /tmp/qemu-entitlements.xml /opt/zerobrew/bin/qemu-system-aarch64
fi

# Workaround 2: zerobrew leaves pcre2 unlinked when two versions exist,
# but glib hardcodes /opt/zerobrew/opt/pcre2. Link the newest.
if [ ! -e /opt/zerobrew/opt/pcre2 ]; then
  echo "==> linking pcre2"
  latest="$(ls -d /opt/zerobrew/Cellar/pcre2/* | sort -V | tail -n 1)"
  ln -s "$latest" /opt/zerobrew/opt/pcre2
fi

# Compose v2 plugin for the zerobrew docker CLI.
mkdir -p ~/.docker/cli-plugins
ln -sf /opt/zerobrew/bin/docker-compose ~/.docker/cli-plugins/docker-compose

if ! colima status >/dev/null 2>&1; then
  echo "==> starting colima (qemu driver)"
  colima start --vm-type qemu --cpu 4 --memory 8
fi

echo "==> verify"
go version
docker --version
docker compose version
colima status
gofmt -l ./cmd ./internal ./tests
go vet ./...
go test -count=1 ./...
cp deploy/.env.example deploy/.env
docker compose --env-file deploy/.env -f deploy/compose.yml config >/dev/null
rm deploy/.env
echo "dev toolchain OK"
