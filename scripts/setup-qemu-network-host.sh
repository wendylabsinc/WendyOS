#!/usr/bin/env bash
#
# Setup TAP interface and DHCP for QEMU WendyOS testing
# This creates the same network topology as USB gadget internet sharing
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m' # No Color

# Global flags
VERBOSE=false
DRY_RUN=false

# Network configuration
TAP="tap-wendyos"
BRIDGE="br-wendyos"
HOST_IP="10.42.0.1/24"
DHCP_RANGE="10.42.0.10,10.42.0.250"
DNSMASQ_PID="/var/run/dnsmasq-wendyos.pid"
STATE_FILE="/var/run/wendyos-qemu-network.state"

# Print formatted/colored output
info() { printf "%b\n" "$*"; }
success() { printf "%b\n" "$*"; }
warning() { printf "%bWARNING%b: %s\n" "${YELLOW}" "${NC}" "$*"; }
error() { printf "%bERROR%b: %s\n" "${RED}" "${NC}" "$*"; }
debug() {
    if [[ "${VERBOSE}" == "true" ]]; then
        printf "%bDEBUG%b: %s\n" "${BLUE}" "${NC}" "$*" >&2
    fi
}

# Execute command or show what would be executed in dry-run mode
execute() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        printf "%bDRY-RUN%b: %s\n" "${YELLOW}" "${NC}" "$*"
        return 0
    else
        eval "$*"
    fi
}

# Check if a command is available
check_command() {
    command -v "$1" &> /dev/null
}

# Check for all required tools
check_required_tools() {
    local missing_tools=()
    local required_tools=(
        "ip"         # Network interface management
        "sudo"       # Elevated privileges
        "dnsmasq"    # DHCP server
        "iptables"   # Firewall/NAT
        "sysctl"     # Kernel parameters
        "ss"         # Socket statistics (for port checking)
        "ps"         # Process status
    )

    for tool in "${required_tools[@]}"
    do
        if ! check_command "${tool}"
        then
            missing_tools+=("${tool}")
        fi
    done

    if [[ ${#missing_tools[@]} -gt 0 ]]
    then
        error "Missing required tools: ${missing_tools[*]}"
        echo ""
        echo "Please install the missing tools and try again."
        echo "On Debian/Ubuntu: sudo apt install ${missing_tools[*]}"
        echo "On Fedora/RHEL:   sudo dnf install ${missing_tools[*]}"
        echo "On Arch:          sudo pacman -S ${missing_tools[*]}"
        exit 1
    fi
}

# Check if we can use sudo
check_sudo() {
    if ! sudo -n true 2>/dev/null
    then
        warning "Some commands require sudo privileges. You may be prompted for your password."
    fi
}

# Check if network is already set up
check_network_status() {
    local bridge_exists=false
    local tap_exists=false
    local dnsmasq_running=false

    if ip link show "${BRIDGE}" &>/dev/null
    then
        bridge_exists=true
    fi

    if ip link show "${TAP}" &>/dev/null
    then
        tap_exists=true
    fi

    if [[ -f "${DNSMASQ_PID}" ]]
    then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -n "${pid}" ]] && ps -p "${pid}" &>/dev/null
        then
            dnsmasq_running=true
        fi
    fi

    if [[ "${bridge_exists}" == "true" ]] && [[ "${tap_exists}" == "true" ]] && [[ "${dnsmasq_running}" == "true" ]]
    then
        return 0  # Network already set up
    else
        return 1  # Network needs setup
    fi
}

# Create TAP interface
create_tap() {
    if ip link show "${TAP}" &>/dev/null
    then
        debug "TAP interface ${TAP} already exists"

        # Check if it's in a bad state and try to bring it up
        if ! ip link show "${TAP}" | grep -q "UP"
        then
            debug "TAP interface is DOWN, bringing it up"
            if ! execute "sudo ip link set '${TAP}' up"
            then
                warning "Failed to bring up existing TAP interface"
                info "Attempting to recreate TAP interface"
                execute "sudo ip link del '${TAP}' 2>/dev/null || true"
            else
                return 0
            fi
        else
            return 0
        fi
    fi

    info "Creating TAP interface: ${BOLD}${TAP}${NC}"
    if ! execute "sudo ip tuntap add dev '${TAP}' mode tap user '${USER}'"
    then
        error "Failed to create TAP interface"
        return 1
    fi

    if ! execute "sudo ip link set '${TAP}' up"
    then
        error "Failed to bring up TAP interface"
        execute "sudo ip link del '${TAP}' 2>/dev/null || true"
        return 1
    fi

    return 0
}

# Create bridge
create_bridge() {
    if ip link show "${BRIDGE}" &>/dev/null
    then
        debug "Bridge ${BRIDGE} already exists"

        # Verify TAP is part of the bridge
        if ! ip link show "${TAP}" | grep -q "master ${BRIDGE}"
        then
            debug "TAP is not part of bridge, adding it"
            if ! execute "sudo ip link set '${TAP}' master '${BRIDGE}'"
            then
                warning "Failed to add TAP to existing bridge"
            fi
        fi

        # Check if bridge has correct IP
        if ! ip addr show "${BRIDGE}" | grep -q "${HOST_IP}"
        then
            debug "Bridge missing IP address, adding it"
            execute "sudo ip addr add '${HOST_IP}' dev '${BRIDGE}' 2>/dev/null || true"
        fi

        # Ensure bridge is up
        if ! ip link show "${BRIDGE}" | grep -q "UP"
        then
            debug "Bridge is DOWN, bringing it up"
            execute "sudo ip link set '${BRIDGE}' up"
        fi

        return 0
    fi

    info "Creating bridge: ${BOLD}${BRIDGE}${NC}"
    if ! execute "sudo ip link add '${BRIDGE}' type bridge"
    then
        error "Failed to create bridge"
        return 1
    fi

    if ! execute "sudo ip link set '${TAP}' master '${BRIDGE}'"
    then
        error "Failed to add TAP to bridge"
        execute "sudo ip link del '${BRIDGE}' 2>/dev/null || true"
        return 1
    fi

    # Add IP address (ignore if already exists)
    if ! execute "sudo ip addr add '${HOST_IP}' dev '${BRIDGE}' 2>/dev/null"
    then
        # Check if IP already exists on the bridge (not an error)
        if ip addr show "${BRIDGE}" | grep -q "${HOST_IP}"
        then
            debug "IP address already assigned to bridge"
        else
            error "Failed to assign IP to bridge"
            execute "sudo ip link del '${BRIDGE}' 2>/dev/null || true"
            return 1
        fi
    fi

    if ! execute "sudo ip link set '${BRIDGE}' up"
    then
        error "Failed to bring up bridge"
        execute "sudo ip link del '${BRIDGE}' 2>/dev/null || true"
        return 1
    fi

    return 0
}

# Save current IP forwarding state
save_ip_forward_state() {
    local current
    current=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo "0")

    if [[ ! -f "${STATE_FILE}" ]]
    then
        if [[ "${DRY_RUN}" == "false" ]]
        then
            echo "ip_forward_original=${current}" | sudo tee "${STATE_FILE}" > /dev/null
            debug "Saved IP forwarding state: ${current}"
        fi
    fi
}

# Enable IP forwarding
enable_forwarding() {
    local current
    current=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo "0")

    if [[ "${current}" == "1" ]]
    then
        debug "IP forwarding already enabled"
        save_ip_forward_state
        return 0
    fi

    warning "Enabling IP forwarding (global system setting)"
    info "This allows packet forwarding between network interfaces"

    # Save original state before changing
    save_ip_forward_state

    if ! execute "sudo sysctl -w net.ipv4.ip_forward=1"
    then
        error "Failed to enable IP forwarding"
        return 1
    fi

    return 0
}

# Check if dnsmasq is already serving the bridge
check_existing_dnsmasq() {
    # Get bridge IP
    local bridge_ip
    bridge_ip=$(ip addr show "${BRIDGE}" 2>/dev/null | grep "inet " | awk '{print $2}' | cut -d/ -f1)

    if [[ -z "${bridge_ip}" ]]
    then
        return 1  # Bridge has no IP, no dnsmasq can be serving it
    fi

    # Check if any dnsmasq process is listening on port 53 on the bridge IP
    if sudo ss -ulnp | grep "dnsmasq" | grep -q "${bridge_ip}:53"
    then
        return 0  # dnsmasq already serving this bridge
    fi

    # Also check by process command line (in case ss doesn't show bridge name)
    if ps aux | grep "[d]nsmasq.*--interface.*${BRIDGE}" &>/dev/null
    then
        return 0  # dnsmasq configured for this bridge
    fi

    return 1  # No dnsmasq on this bridge
}

# Check if port 53 is available on the bridge
check_port_available() {
    # Get bridge IP
    local bridge_ip
    bridge_ip=$(ip addr show "${BRIDGE}" 2>/dev/null | grep "inet " | awk '{print $2}' | cut -d/ -f1)

    if [[ -z "${bridge_ip}" ]]; then
        debug "Bridge has no IP yet, port check skipped"
        return 0
    fi

    # Check if anything is listening on port 53 on the bridge IP
    if sudo ss -ulnp | grep -q ":53.*${bridge_ip}"; then
        return 1  # Port in use
    fi

    return 0  # Port available
}

# Start DHCP server
start_dnsmasq() {
    # Check if our dnsmasq is already running
    if [[ -f "${DNSMASQ_PID}" ]]; then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -n "${pid}" ]] && ps -p "${pid}" &>/dev/null; then
            debug "dnsmasq already running (PID: ${pid})"
            return 0
        else
            debug "Stale PID file found, removing"
            execute "sudo rm -f '${DNSMASQ_PID}'"
        fi
    fi

    # Check if bridge exists
    if ! ip link show "${BRIDGE}" &>/dev/null; then
        error "Bridge ${BRIDGE} does not exist, cannot start dnsmasq"
        return 1
    fi

    # Check if another dnsmasq is already serving this bridge
    if check_existing_dnsmasq; then
        warning "Another dnsmasq is already serving ${BRIDGE}"
        info "Assuming it's configured correctly for QEMU network"
        return 0
    fi

    # Check if port 53 is available
    if ! check_port_available; then
        warning "Port 53 appears to be in use on ${BRIDGE}"
        info "Attempting to start dnsmasq anyway (--bind-interfaces may allow it)"
    fi

    info "Starting DHCP server (dnsmasq)"
    local cmd="sudo dnsmasq \
        --interface='${BRIDGE}' \
        --bind-interfaces \
        --dhcp-range='${DHCP_RANGE},12h' \
        --dhcp-option=3,10.42.0.1 \
        --dhcp-option=6,10.42.0.1 \
        --pid-file='${DNSMASQ_PID}' \
        --log-queries \
        --log-dhcp"

    if [[ "${DRY_RUN}" == "true" ]]; then
        execute "${cmd}"
        return 0
    fi

    # Capture error output
    local error_output
    if ! error_output=$(eval "${cmd}" 2>&1); then
        error "Failed to start dnsmasq"
        echo ""
        echo "Error details:"
        echo "${error_output}" | sed 's/^/  /'
        echo ""

        # Provide helpful diagnostics
        if echo "${error_output}" | grep -q "port 53"; then
            warning "Port 53 is in use. Common causes:"
            echo "  - System-wide dnsmasq running: sudo systemctl status dnsmasq"
            echo "  - systemd-resolved using port 53: sudo systemctl status systemd-resolved"
            echo "  - Another DNS server running: sudo ss -ulnp | grep :53"
            echo ""
            info "Suggestion: Stop conflicting service or configure it to not bind to all interfaces"
        elif echo "${error_output}" | grep -q "bind"; then
            warning "Failed to bind to interface. Check if:"
            echo "  - Bridge ${BRIDGE} exists and is UP: ip link show ${BRIDGE}"
            echo "  - You have sufficient privileges: sudo rights"
        fi

        return 1
    fi

    # Give dnsmasq a moment to start
    sleep 1

    # Verify it started
    if [[ -f "${DNSMASQ_PID}" ]]; then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -n "${pid}" ]] && ps -p "${pid}" &>/dev/null; then
            debug "dnsmasq started successfully (PID: ${pid})"
            return 0
        else
            error "dnsmasq started but process not found"
            return 1
        fi
    else
        error "dnsmasq started but PID file not created"
        return 1
    fi
}

# Enable NAT
enable_nat() {
    # Check if rule already exists
    if sudo iptables -t nat -C POSTROUTING -s 10.42.0.0/24 -j MASQUERADE 2>/dev/null; then
        debug "NAT rule already exists"
        return 0
    fi

    info "Setting up NAT for internet access"
    if ! execute "sudo iptables -t nat -A POSTROUTING -s 10.42.0.0/24 -j MASQUERADE"; then
        error "Failed to add NAT rule"
        return 1
    fi

    return 0
}

# Verify setup
verify_setup() {
    if [[ "${DRY_RUN}" == "true" ]]; then
        return 0
    fi

    local errors=0

    # Check bridge
    if ! ip link show "${BRIDGE}" | grep -q "UP"; then
        error "Bridge ${BRIDGE} is not UP"
        errors=$((errors + 1))
    fi

    # Check TAP
    if ! ip link show "${TAP}" | grep -q "UP"; then
        error "TAP ${TAP} is not UP"
        errors=$((errors + 1))
    fi

    # Check dnsmasq
    if [[ -f "${DNSMASQ_PID}" ]]; then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -z "${pid}" ]] || ! ps -p "${pid}" &>/dev/null; then
            error "dnsmasq is not running"
            errors=$((errors + 1))
        fi
    else
        error "dnsmasq PID file not found"
        errors=$((errors + 1))
    fi

    if [[ ${errors} -gt 0 ]]; then
        return 1
    fi

    return 0
}

# Show status
show_status() {
    local bridge_up=false
    local tap_up=false
    local dnsmasq_running=false
    local ip_forward=false

    info "QEMU Network Status:"
    echo ""

    # Check bridge
    if ip link show "${BRIDGE}" &>/dev/null; then
        if ip link show "${BRIDGE}" | grep -q "UP"; then
            bridge_up=true
            info "  Bridge:       ${GREEN}${BOLD}${BRIDGE}${NC} ${GREEN}[UP]${NC}"
            local bridge_ip
            bridge_ip=$(ip addr show "${BRIDGE}" | grep "inet " | awk '{print $2}' | head -1)
            if [[ -n "${bridge_ip}" ]]; then
                info "  Bridge IP:    ${BOLD}${bridge_ip}${NC}"
            fi
        else
            info "  Bridge:       ${YELLOW}${BOLD}${BRIDGE}${NC} ${YELLOW}[DOWN]${NC}"
        fi
    else
        info "  Bridge:       ${RED}Not created${NC}"
    fi

    # Check TAP
    if ip link show "${TAP}" &>/dev/null; then
        if ip link show "${TAP}" | grep -q "UP"; then
            tap_up=true
            info "  TAP:          ${GREEN}${BOLD}${TAP}${NC} ${GREEN}[UP]${NC}"
        else
            info "  TAP:          ${YELLOW}${BOLD}${TAP}${NC} ${YELLOW}[DOWN]${NC}"
        fi
    else
        info "  TAP:          ${RED}Not created${NC}"
    fi

    # Check dnsmasq
    if [[ -f "${DNSMASQ_PID}" ]]; then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -n "${pid}" ]] && ps -p "${pid}" &>/dev/null; then
            dnsmasq_running=true
            info "  DHCP server:  ${GREEN}${BOLD}Running${NC} (PID: ${pid})"
            info "  DHCP range:   ${BOLD}${DHCP_RANGE}${NC}"
        else
            info "  DHCP server:  ${RED}Not running${NC}"
        fi
    else
        info "  DHCP server:  ${RED}Not running${NC}"
    fi

    # Check IP forwarding
    local forward_value
    forward_value=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo "0")
    if [[ "${forward_value}" == "1" ]]; then
        ip_forward=true
        info "  IP forward:   ${GREEN}${BOLD}Enabled${NC}"
    else
        info "  IP forward:   ${RED}Disabled${NC}"
    fi

    # Check NAT
    if sudo iptables -t nat -C POSTROUTING -s 10.42.0.0/24 -j MASQUERADE 2>/dev/null; then
        info "  NAT:          ${GREEN}${BOLD}Enabled${NC}"
    else
        info "  NAT:          ${RED}Not configured${NC}"
    fi

    echo ""

    # Check for potential issues
    local issues_found=false

    if [[ "${bridge_up}" == "true" ]] && [[ "${tap_up}" == "true" ]] && [[ "${dnsmasq_running}" == "true" ]] && [[ "${ip_forward}" == "true" ]]; then
        success "✓ QEMU network is ready!"
        echo ""
        echo "QEMU guests will receive IPs from ${DHCP_RANGE}"
    else
        warning "QEMU network is not fully configured"
        echo ""
        issues_found=true

        # Provide specific guidance
        if [[ "${bridge_up}" == "false" ]]; then
            info "  Issue: Bridge not configured"
            echo "  Action: Run '$0 setup' to create the bridge"
        fi

        if [[ "${tap_up}" == "false" ]]; then
            info "  Issue: TAP interface not configured"
            echo "  Action: Run '$0 setup' to create the TAP interface"
        fi

        if [[ "${dnsmasq_running}" == "false" ]]; then
            info "  Issue: DHCP server not running"
            echo "  Action: Check for port conflicts: sudo ss -ulnp | grep ':53'"
            echo "          Then run: $0 setup"
        fi

        if [[ "${ip_forward}" == "false" ]]; then
            info "  Issue: IP forwarding disabled"
            echo "  Action: Enable with: sudo sysctl -w net.ipv4.ip_forward=1"
        fi
    fi

    # Check for additional potential issues
    if [[ "${issues_found}" == "false" ]]; then
        # Check if there are conflicting dnsmasq instances
        local other_dnsmasq
        other_dnsmasq=$(ps aux | grep "[d]nsmasq" | grep -v "${DNSMASQ_PID}" | wc -l)
        if [[ ${other_dnsmasq} -gt 0 ]]; then
            echo ""
            info "Note: Found ${other_dnsmasq} other dnsmasq instance(s) running"
            debug "This may be normal (system services) but could cause conflicts"
        fi
    fi
}

# Restore original IP forwarding state
restore_ip_forward_state() {
    if [[ ! -f "${STATE_FILE}" ]]; then
        debug "No state file found, IP forwarding not modified by us"
        return 0
    fi

    local original_state
    original_state=$(grep "ip_forward_original=" "${STATE_FILE}" 2>/dev/null | cut -d= -f2)

    if [[ -z "${original_state}" ]]; then
        debug "Could not read original IP forwarding state"
        return 0
    fi

    local current
    current=$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo "0")

    if [[ "${current}" != "${original_state}" ]]; then
        info "Restoring IP forwarding to original state: ${original_state}"
        if execute "sudo sysctl -w net.ipv4.ip_forward=${original_state}"; then
            success "IP forwarding restored to: ${original_state}"
        else
            warning "Failed to restore IP forwarding state"
        fi
    else
        debug "IP forwarding already at original state: ${original_state}"
    fi

    # Remove state file
    execute "sudo rm -f '${STATE_FILE}' 2>/dev/null || true"
}

# Cleanup network
cleanup_network() {
    info "Cleaning up QEMU network..."
    echo ""

    local cleanup_errors=0

    # Stop dnsmasq
    if [[ -f "${DNSMASQ_PID}" ]]; then
        local pid
        pid=$(cat "${DNSMASQ_PID}" 2>/dev/null || echo "")
        if [[ -n "${pid}" ]] && ps -p "${pid}" &>/dev/null; then
            info "Stopping dnsmasq (PID: ${pid})"
            if execute "sudo kill '${pid}'"; then
                sleep 1
                # Force kill if still running
                if ps -p "${pid}" &>/dev/null; then
                    debug "Process still running, sending SIGKILL"
                    execute "sudo kill -9 '${pid}' 2>/dev/null || true"
                fi
            else
                warning "Failed to stop dnsmasq process"
                cleanup_errors=$((cleanup_errors + 1))
            fi
        fi
        execute "sudo rm -f '${DNSMASQ_PID}' 2>/dev/null || true"
    else
        # Check for any dnsmasq process on our bridge without PID file
        local orphan_pid
        orphan_pid=$(ps aux | grep "[d]nsmasq.*${BRIDGE}" | awk '{print $2}' | head -1)
        if [[ -n "${orphan_pid}" ]]; then
            warning "Found orphaned dnsmasq process (PID: ${orphan_pid})"
            info "Stopping orphaned dnsmasq"
            execute "sudo kill '${orphan_pid}' 2>/dev/null || true"
            sleep 1
            if ps -p "${orphan_pid}" &>/dev/null; then
                execute "sudo kill -9 '${orphan_pid}' 2>/dev/null || true"
            fi
        fi
    fi

    # Remove NAT rule
    if sudo iptables -t nat -C POSTROUTING -s 10.42.0.0/24 -j MASQUERADE 2>/dev/null; then
        info "Removing NAT rule"
        if ! execute "sudo iptables -t nat -D POSTROUTING -s 10.42.0.0/24 -j MASQUERADE"; then
            warning "Failed to remove NAT rule"
            cleanup_errors=$((cleanup_errors + 1))
        fi
    else
        debug "NAT rule not found (already removed or never created)"
    fi

    # Remove bridge (this will automatically remove attached interfaces)
    if ip link show "${BRIDGE}" &>/dev/null; then
        info "Removing bridge ${BRIDGE}"

        # First, bring it down
        if ! execute "sudo ip link set '${BRIDGE}' down 2>/dev/null"; then
            warning "Failed to bring down bridge (may already be down)"
        fi

        # Remove the bridge
        if ! execute "sudo ip link del '${BRIDGE}' 2>/dev/null"; then
            warning "Failed to remove bridge"
            cleanup_errors=$((cleanup_errors + 1))
        fi
    else
        debug "Bridge not found (already removed or never created)"
    fi

    # Remove TAP (if bridge removal didn't already remove it)
    if ip link show "${TAP}" &>/dev/null; then
        info "Removing TAP ${TAP}"
        if ! execute "sudo ip link del '${TAP}' 2>/dev/null"; then
            warning "Failed to remove TAP"
            cleanup_errors=$((cleanup_errors + 1))
        fi
    else
        debug "TAP not found (already removed)"
    fi

    # Restore IP forwarding state
    echo ""
    restore_ip_forward_state

    echo ""

    if [[ "${DRY_RUN}" == "true" ]]; then
        success "Would clean up QEMU network"
        return 0
    fi

    if [[ ${cleanup_errors} -eq 0 ]]; then
        success "✓ QEMU network cleaned up successfully"
        return 0
    else
        warning "QEMU network cleanup completed with ${cleanup_errors} error(s)"
        echo ""
        info "Some components may not have been removed. You can:"
        echo "  - Check remaining state: $0 status"
        echo "  - Manually clean up: sudo ip link del ${BRIDGE} 2>/dev/null"
        echo "  - Check processes: ps aux | grep dnsmasq"
        return 1
    fi
}

# Check for potential conflicts
check_conflicts() {
    local conflicts_found=false

    # Check if 10.42.0.0/24 is already in use on another interface
    local existing_10_42
    existing_10_42=$(ip addr show | grep "inet 10.42.0" | grep -v "${BRIDGE}" || echo "")

    if [[ -n "${existing_10_42}" ]]; then
        warning "IP range 10.42.0.0/24 is already in use on your system:"
        echo "${existing_10_42}" | sed 's/^/  /'
        echo ""
        warning "This may cause routing conflicts with QEMU networking"
        echo ""
        conflicts_found=true
    fi

    if [[ "${conflicts_found}" == "true" ]]; then
        # Check if running interactively
        if [[ -t 0 ]]; then
            echo "Continue anyway? (y/N)"
            read -r response
            if [[ ! "${response}" =~ ^[Yy]$ ]]; then
                info "Setup cancelled by user"
                exit 0
            fi
            echo ""
        else
            # Non-interactive mode - warn and proceed
            warning "Running in non-interactive mode, proceeding despite conflicts"
            echo ""
        fi
    fi
}

# Setup network
setup_network() {
    info "Setting up QEMU network for WendyOS..."
    # echo ""

    # Check for potential conflicts
    check_conflicts

    # Check if already set up
    if check_network_status; then
        success "✓ QEMU network already configured"
        echo ""
        show_status
        return 0
    fi

    local setup_failed=false
    local failed_component=""

    # Create TAP (fail if this fails)
    if ! create_tap; then
        error "Failed to create TAP interface"
        failed_component="TAP"
        setup_failed=true
    fi

    # Create bridge (fail if this fails)
    if [[ "${setup_failed}" == "false" ]] && ! create_bridge; then
        error "Failed to create bridge"
        failed_component="Bridge"
        setup_failed=true
    fi

    # Enable forwarding (warn but continue if this fails)
    if [[ "${setup_failed}" == "false" ]]; then
        if ! enable_forwarding; then
            warning "Failed to enable IP forwarding (non-critical)"
        fi
    fi

    # Start dnsmasq (fail if this fails)
    if [[ "${setup_failed}" == "false" ]] && ! start_dnsmasq; then
        error "Failed to start dnsmasq"
        failed_component="dnsmasq"
        setup_failed=true
    fi

    # Enable NAT (warn but continue if this fails)
    if [[ "${setup_failed}" == "false" ]]; then
        if ! enable_nat; then
            warning "Failed to enable NAT (non-critical, may already be configured)"
        fi
    fi

    echo ""

    # Handle setup failure
    if [[ "${setup_failed}" == "true" ]]; then
        error "Network setup failed at: ${failed_component}"
        echo ""
        warning "Partial setup may exist. Run '$0 cleanup' to remove."
        echo ""
        info "Troubleshooting steps:"
        echo "  1. Check system logs: sudo journalctl -xe"
        echo "  2. Check network state: ip link show"
        echo "  3. Check for conflicts: sudo ss -ulnp | grep ':53'"
        echo "  4. Try cleanup and retry: $0 cleanup && $0 setup"
        return 1
    fi

    # Verify
    if verify_setup; then
        success "✓ QEMU network setup complete!"
        echo ""
        echo "Configuration:"
        echo "  Bridge:     ${BRIDGE} at 10.42.0.1"
        echo "  DHCP range: ${DHCP_RANGE}"
        echo "  TAP:        ${TAP}"
        echo ""
        echo "QEMU guests will receive IPs from 10.42.0.10-250"
        return 0
    else
        error "Network setup verification failed"
        echo ""
        warning "Setup completed but verification found issues."
        echo ""
        show_status
        return 1
    fi
}

# Show usage
show_usage() {
    cat << EOF
Usage: $0 [options] <command>

Commands:
    setup    Setup QEMU network (TAP, bridge, DHCP)
    status   Show current network status
    cleanup  Remove QEMU network configuration

Options:
    -v, --verbose   Enable verbose/debug output
    -n, --dry-run   Show what would be done without doing it
    -h, --help      Show this help message

Examples:
    $0 setup                    # Setup QEMU network
    $0 --verbose setup          # Setup with debug output
    $0 --dry-run setup          # Preview what would be done
    $0 status                   # Show current status
    $0 cleanup                  # Remove network configuration

EOF
}

# Main script logic
main() {
    local command=""

    # Parse flags and arguments
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -v|--verbose)
                VERBOSE=true
                shift
                ;;
            -n|--dry-run)
                DRY_RUN=true
                shift
                ;;
            -h|--help)
                show_usage
                exit 0
                ;;
            setup|status|cleanup)
                command="$1"
                shift
                ;;
            *)
                error "Unknown argument: $1"
                echo ""
                show_usage
                exit 1
                ;;
        esac
    done

    # Default to setup if no command provided
    if [[ -z "${command}" ]]; then
        command="setup"
    fi

    debug "Verbose mode enabled"
    if [[ "${DRY_RUN}" == "true" ]]; then
        info "Dry run mode enabled - no changes will be made"
        echo ""
    fi

    # Check required tools
    check_required_tools

    # Check sudo availability (all commands may need sudo)
    check_sudo

    case "${command}" in
        setup)
            setup_network
            ;;
        status)
            show_status
            ;;
        cleanup)
            cleanup_network
            ;;
        *)
            error "Unknown command: ${command}"
            echo ""
            show_usage
            exit 1
            ;;
    esac
}

main "$@"
