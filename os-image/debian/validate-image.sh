#!/bin/bash
# Statically validate a locally loaded Cocoon-compatible Debian 13 image.
set -o errexit
set -o nounset
set -o pipefail

IMAGE=""
CONTAINER_ENGINE="${CONTAINER_ENGINE:-}"
IMAGE_ARCHITECTURE=""
EXPECTED_TARGET_PLATFORM=""
EXPECTED_KERNEL_VERSION_SUFFIX=""
SCRIPT_DIR=""
TEMP_DIR=""
LAST_CONTAINER=""
FAILURES=0
declare -a CONTAINER_IDS=()

log() {
  printf '%s\n' "$*" >&2
}

fatal() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

record_failure() {
  printf 'FAIL: %s\n' "$*" >&2
  FAILURES=$((FAILURES + 1))
}

usage() {
  printf 'Usage: %s IMAGE\n' "${0##*/}" >&2
}

cleanup() {
  local status=$?
  local container_id

  trap - EXIT
  set +o errexit
  for container_id in "${CONTAINER_IDS[@]}"; do
    [[ -n "$container_id" ]] && container_cli rm --force "$container_id" >/dev/null 2>&1
  done
  [[ -n "$TEMP_DIR" ]] && rm -rf -- "$TEMP_DIR"
  exit "$status"
}

container_cli() {
  "$CONTAINER_ENGINE" "$@"
}

select_container_engine() {
  local candidate

  if [[ -n "$CONTAINER_ENGINE" ]]; then
    command -v "$CONTAINER_ENGINE" >/dev/null 2>&1 \
      || fatal "configured container engine not found: $CONTAINER_ENGINE"
    return
  fi

  for candidate in podman docker; do
    if command -v "$candidate" >/dev/null 2>&1; then
      CONTAINER_ENGINE=$candidate
      return
    fi
  done
  fatal "required command not found: podman or docker"
}

check_prerequisites() {
  local command_name
  local -a required_commands=(awk cmp dirname find grep mkdir mktemp readlink rm)

  for command_name in "${required_commands[@]}"; do
    command -v "$command_name" >/dev/null 2>&1 || fatal "required command not found: $command_name"
  done
  select_container_engine
  container_cli info >/dev/null 2>&1 || fatal "$CONTAINER_ENGINE is unavailable"

  for command_name in overlay.sh network.sh; do
    [[ -r "$SCRIPT_DIR/$command_name" ]] || fatal "missing vendored input: $SCRIPT_DIR/$command_name"
  done
}

inspect_image() {
  local format=$1
  container_cli image inspect --format "$format" "$IMAGE"
}

create_container() {
  LAST_CONTAINER=$(container_cli create "$@") || fatal "could not create validation container from $IMAGE"
  [[ -n "$LAST_CONTAINER" ]] || fatal "$CONTAINER_ENGINE create returned an empty container ID"
  CONTAINER_IDS+=("$LAST_CONTAINER")
}

assert_equal() {
  local description=$1
  local expected=$2
  local actual=$3

  if [[ "$actual" != "$expected" ]]; then
    record_failure "$description (expected '$expected', got '$actual')"
  fi
}

assert_regular_file() {
  local path=$1
  local description=$2

  if [[ ! -f "$path" || -L "$path" ]]; then
    record_failure "$description is not a regular file"
  fi
}

assert_nonempty_file() {
  local path=$1
  local description=$2

  if [[ ! -s "$path" || -L "$path" ]]; then
    record_failure "$description is missing or empty"
  fi
}

assert_contains_fixed() {
  local path=$1
  local expected=$2
  local description=$3

  if [[ ! -f "$path" ]] || ! grep -Fq -- "$expected" "$path"; then
    record_failure "$description"
  fi
}

assert_contains_line() {
  local path=$1
  local expected=$2
  local description=$3

  if [[ ! -f "$path" ]] || ! grep -Fxq -- "$expected" "$path"; then
    record_failure "$description"
  fi
}

assert_contains_regex() {
  local path=$1
  local expression=$2
  local description=$3

  if [[ ! -f "$path" ]] || ! grep -Eq -- "$expression" "$path"; then
    record_failure "$description"
  fi
}

assert_enabled_service() {
  local rootfs=$1
  local unit=$2
  local match

  match=$(find "$rootfs/etc/systemd/system" -type l -path '*.wants/*' -name "$unit" -print -quit 2>/dev/null || true)
  [[ -n "$match" ]] || record_failure "$unit is not enabled"
}

assert_masked_service() {
  local rootfs=$1
  local unit=$2
  local path="$rootfs/etc/systemd/system/$unit"
  local target=""

  if [[ -L "$path" ]]; then
    target=$(readlink "$path")
  fi
  [[ "$target" == "/dev/null" ]] || record_failure "$unit is not masked to /dev/null"
}

assert_kernel_feature() {
  local rootfs=$1
  local kernel_version=$2
  local config=$3
  local initrd_listing=$4
  local symbol=$5
  local module=$6
  local setting
  local module_path

  setting=$(awk -F= -v symbol="$symbol" '$1 == symbol { print $2; exit }' "$config")
  case "$setting" in
    y)
      ;;
    m)
      module_path=$(find "$rootfs/usr/lib/modules/$kernel_version" -type f -name "${module}.ko*" -print -quit 2>/dev/null || true)
      [[ -n "$module_path" ]] || record_failure "$symbol=m but module $module is absent for $kernel_version"
      grep -Eq "(^|/)${module}\\.ko(\\.(gz|xz|zst))?$" "$initrd_listing" \
        || record_failure "$symbol=m but module $module is absent from initrd.img-$kernel_version"
      ;;
    *)
      record_failure "$symbol is neither built in nor modular for $kernel_version"
      ;;
  esac
}

copy_image_filesystem() {
  local rootfs=$1
  local container_id
  local path

  create_container --entrypoint /bin/true "$IMAGE"
  container_id=$LAST_CONTAINER
  for path in boot etc usr sbin; do
    container_cli cp "${container_id}:/${path}" "$rootfs/" \
      || fatal "could not copy /$path from validation container $container_id"
  done
}

select_architecture_contract() {
  local operating_system=$1
  local architecture=$2

  [[ "$operating_system" == "linux" ]] \
    || fatal "unsupported OCI operating system '$operating_system' (expected linux)"

  IMAGE_ARCHITECTURE=$architecture
  EXPECTED_TARGET_PLATFORM="linux/$architecture"
  case "$architecture" in
    amd64)
      EXPECTED_KERNEL_VERSION_SUFFIX=-cloud-amd64
      ;;
    arm64)
      EXPECTED_KERNEL_VERSION_SUFFIX=-cloud-arm64
      ;;
    *)
      fatal "unsupported OCI architecture '$architecture' (expected amd64 or arm64)"
      ;;
  esac
}

validate_image_metadata() {
  local architecture
  local operating_system
  local command_metadata
  local entrypoint_metadata
  local user_metadata

  container_cli image inspect "$IMAGE" >/dev/null 2>&1 || fatal "image is not present locally: $IMAGE"

  architecture=$(inspect_image '{{.Architecture}}')
  operating_system=$(inspect_image '{{.Os}}')
  command_metadata=$(inspect_image '{{json .Config.Cmd}}')
  entrypoint_metadata=$(inspect_image '{{json .Config.Entrypoint}}')
  user_metadata=$(inspect_image '{{.Config.User}}')

  select_architecture_contract "$operating_system" "$architecture"
  assert_equal "OCI architecture" "$IMAGE_ARCHITECTURE" "$architecture"
  assert_equal "OCI operating system" "linux" "$operating_system"
  assert_equal "OCI command" '["/sbin/init"]' "$command_metadata"
  assert_equal "OCI entrypoint" "null" "$entrypoint_metadata"
  case "$user_metadata" in
    ''|0|root) ;;
    *) record_failure "OCI user must be root (got '$user_metadata')" ;;
  esac
}

validate_identity_and_repositories() {
  local rootfs=$1
  local apt_sources="$TEMP_DIR/apt-sources"

  assert_contains_regex "$rootfs/usr/lib/os-release" '^ID=debian$' "guest identity is not Debian"
  assert_contains_regex "$rootfs/usr/lib/os-release" '^VERSION_ID="?13"?$' "guest version is not Debian 13"
  assert_contains_regex "$rootfs/usr/lib/os-release" '^VERSION_CODENAME=trixie$' "guest codename is not trixie"

  # Official Debian images retain snapshot URLs in comments to document how
  # the base rootfs was produced. Validate only enabled APT source lines.
  find "$rootfs/etc/apt" -type f \( -name '*.list' -o -name '*.sources' \) \
    -exec awk '!/^[[:space:]]*#/' {} + > "$apt_sources"
  assert_contains_fixed "$apt_sources" 'deb.debian.org' "APT sources are not the moving Debian repositories"
  assert_contains_fixed "$apt_sources" 'trixie' "APT sources do not select trixie"
  if grep -Fq 'snapshot.debian.org' "$apt_sources"; then
    record_failure "APT sources unexpectedly claim snapshot policy"
  fi
}

validate_boot_contract() {
  local rootfs=$1
  local modules_file="$rootfs/etc/initramfs-tools/modules"
  local config
  local initrd
  local initrd_container
  local initrd_listing
  local kernel
  local kernel_version
  local module
  local -a kernels=("$rootfs"/boot/vmlinuz-*)
  local -a required_modules=(
    erofs
    overlay
    ext4
    virtio_blk
    virtio_pci
    virtio_ring
    virtio_net
    vsock
    vmw_vsock_virtio_transport
  )

  for module in "${required_modules[@]}"; do
    assert_contains_line "$modules_file" "$module" "required initramfs module $module is not configured"
  done

  if [[ ${#kernels[@]} -eq 1 && ! -e "${kernels[0]}" ]]; then
    record_failure "no versioned kernel was installed in /boot"
    return
  fi

  for kernel in "${kernels[@]}"; do
    kernel_version=${kernel##*/vmlinuz-}
    [[ "$kernel_version" == *"$EXPECTED_KERNEL_VERSION_SUFFIX" ]] \
      || record_failure "kernel $kernel_version does not match $IMAGE_ARCHITECTURE cloud-kernel naming"
    config="$rootfs/boot/config-$kernel_version"
    initrd="$rootfs/boot/initrd.img-$kernel_version"
    initrd_listing="$TEMP_DIR/initramfs-$kernel_version.list"

    assert_nonempty_file "$kernel" "kernel /boot/vmlinuz-$kernel_version"
    assert_nonempty_file "$initrd" "initramfs /boot/initrd.img-$kernel_version"
    assert_nonempty_file "$config" "kernel configuration /boot/config-$kernel_version"
    [[ -d "$rootfs/usr/lib/modules/$kernel_version" ]] \
      || record_failure "module tree is missing for $kernel_version"

    if [[ ! -s "$initrd" ]]; then
      continue
    fi
    create_container --entrypoint /usr/bin/lsinitramfs "$IMAGE" "/boot/initrd.img-$kernel_version"
    initrd_container=$LAST_CONTAINER
    if ! container_cli start --attach "$initrd_container" > "$initrd_listing"; then
      record_failure "lsinitramfs failed for /boot/initrd.img-$kernel_version"
      continue
    fi

    grep -Fxq 'scripts/cocoon-overlay' "$initrd_listing" \
      || record_failure "cocoon-overlay hook is absent from initrd.img-$kernel_version"
    grep -Fxq 'scripts/init-bottom/cocoon-network' "$initrd_listing" \
      || record_failure "cocoon-network hook is absent from initrd.img-$kernel_version"
    grep -Fxq 'usr/bin/busybox' "$initrd_listing" \
      || record_failure "busybox is absent from initrd.img-$kernel_version"

    if [[ -f "$config" ]]; then
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_EROFS_FS erofs
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_OVERLAY_FS overlay
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_EXT4_FS ext4
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VIRTIO_BLK virtio_blk
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VIRTIO_PCI virtio_pci
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VIRTIO virtio_ring
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VIRTIO_NET virtio_net
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VSOCKETS vsock
      assert_kernel_feature "$rootfs" "$kernel_version" "$config" "$initrd_listing" CONFIG_VIRTIO_VSOCKETS vmw_vsock_virtio_transport
    fi
  done

  [[ -e "$rootfs/sbin/init" || -L "$rootfs/sbin/init" ]] \
    || record_failure "/sbin/init is absent"
  [[ -x "$rootfs/usr/lib/systemd/systemd" ]] \
    || record_failure "systemd executable is absent"

  assert_nonempty_file "$rootfs/etc/initramfs-tools/scripts/cocoon-overlay" "installed cocoon-overlay hook"
  assert_nonempty_file "$rootfs/etc/initramfs-tools/scripts/init-bottom/cocoon-network" "installed cocoon-network hook"
  if [[ -f "$rootfs/etc/initramfs-tools/scripts/cocoon-overlay" ]] \
    && ! cmp -s "$SCRIPT_DIR/overlay.sh" "$rootfs/etc/initramfs-tools/scripts/cocoon-overlay"; then
    record_failure "installed cocoon-overlay hook differs from the vendored source"
  fi
  if [[ -f "$rootfs/etc/initramfs-tools/scripts/init-bottom/cocoon-network" ]] \
    && ! cmp -s "$SCRIPT_DIR/network.sh" "$rootfs/etc/initramfs-tools/scripts/init-bottom/cocoon-network"; then
    record_failure "installed cocoon-network hook differs from the vendored source"
  fi
  [[ -x "$rootfs/etc/initramfs-tools/scripts/cocoon-overlay" ]] \
    || record_failure "installed cocoon-overlay hook is not executable"
  [[ -x "$rootfs/etc/initramfs-tools/scripts/init-bottom/cocoon-network" ]] \
    || record_failure "installed cocoon-network hook is not executable"

  assert_contains_regex "$rootfs/etc/initramfs-tools/initramfs.conf" '^COMPRESS=gzip$' "initramfs compression is not gzip"
  assert_contains_regex "$rootfs/etc/initramfs-tools/initramfs.conf" '^IP=off$' "unrequested initramfs DHCP is not disabled"

  if [[ ! -f "$rootfs/etc/fstab" || -s "$rootfs/etc/fstab" ]]; then
    record_failure "/etc/fstab is not neutralized"
  fi
  assert_masked_service "$rootfs" systemd-fsck-root.service
  assert_masked_service "$rootfs" systemd-remount-fs.service
  assert_masked_service "$rootfs" 'systemd-fsck@.service'
}

validate_network_and_services() {
  local rootfs=$1
  local default_network="$rootfs/etc/systemd/network/20-wired.network"
  local agent_unit="$rootfs/etc/systemd/system/cocoon-agent.service"
  local sshd_development_config="$rootfs/etc/ssh/sshd_config.d/00-cocoon-development.conf"
  local shadow_line
  local root_hash

  assert_regular_file "$default_network" "default networkd configuration"
  assert_contains_regex "$default_network" '^DHCP=yes$' "default networkd configuration does not enable DHCP"
  assert_contains_regex "$default_network" '^ClientIdentifier=mac$' "default networkd DHCP is not clone-safe"

  assert_enabled_service "$rootfs" systemd-networkd.service
  assert_enabled_service "$rootfs" systemd-resolved.service
  assert_enabled_service "$rootfs" systemd-timesyncd.service
  assert_enabled_service "$rootfs" ssh.service
  assert_enabled_service "$rootfs" cocoon-agent.service

  assert_nonempty_file "$rootfs/usr/local/bin/cocoon-agent" "Cocoon agent binary"
  [[ -x "$rootfs/usr/local/bin/cocoon-agent" ]] \
    || record_failure "Cocoon agent binary is not executable"
  assert_contains_fixed "$agent_unit" 'ExecStart=/usr/local/bin/cocoon-agent serve' "Cocoon agent service command is wrong"
  assert_contains_fixed "$agent_unit" 'WantedBy=multi-user.target' "Cocoon agent service has no boot target"

  shadow_line=$(grep '^root:' "$rootfs/etc/shadow" || true)
  root_hash=${shadow_line#root:}
  root_hash=${root_hash%%:*}
  case "$root_hash" in
    ''|\!*|\**)
      record_failure "root account is locked or has no password"
      ;;
  esac
  assert_contains_regex "$sshd_development_config" '^PermitRootLogin[[:space:]]+yes$' "sshd development config does not permit root login"
  assert_contains_regex "$sshd_development_config" '^PasswordAuthentication[[:space:]]+yes$' "sshd development config does not permit password authentication"
}

validate_sshd_effective_config() {
  local sshd_container
  local sshd_config="$TEMP_DIR/sshd-effective-config"

  create_container --entrypoint /usr/sbin/sshd "$IMAGE" -T
  sshd_container=$LAST_CONTAINER
  if ! container_cli start --attach "$sshd_container" > "$sshd_config"; then
    record_failure "sshd could not evaluate its installed configuration"
    return
  fi
  assert_contains_regex "$sshd_config" '^permitrootlogin yes$' "effective sshd configuration denies root login"
  assert_contains_regex "$sshd_config" '^passwordauthentication yes$' "effective sshd configuration denies password authentication"
}

main() {
  local rootfs

  if [[ $# -ne 1 || -z ${1:-} ]]; then
    usage
    exit 2
  fi
  IMAGE=$1
  SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

  check_prerequisites
  trap cleanup EXIT

  validate_image_metadata

  TEMP_DIR=$(mktemp -d)
  rootfs="$TEMP_DIR/rootfs"
  mkdir -p "$rootfs"
  copy_image_filesystem "$rootfs"

  validate_identity_and_repositories "$rootfs"
  validate_boot_contract "$rootfs"
  validate_network_and_services "$rootfs"
  validate_sshd_effective_config

  if [[ $FAILURES -ne 0 ]]; then
    fatal "$IMAGE failed $FAILURES static validation assertion(s)"
  fi
  log "PASS: $IMAGE satisfies the Debian 13 $EXPECTED_TARGET_PLATFORM Cocoon static image contract"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
