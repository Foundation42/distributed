#!/bin/sh
set -e

# Enable IP forwarding for WireGuard
if [ "${ENABLE_WIREGUARD}" = "true" ]; then
    echo "Enabling IP forwarding..."
    sysctl -w net.ipv4.ip_forward=1
    sysctl -w net.ipv6.conf.all.forwarding=1
    
    # Load WireGuard kernel module if available
    if [ -f /lib/modules/$(uname -r)/kernel/drivers/net/wireguard.ko ]; then
        modprobe wireguard || true
    fi
fi

# Create WireGuard interface if needed
if [ "${ENABLE_WIREGUARD}" = "true" ] && [ ! -d /sys/class/net/wg0 ]; then
    echo "Creating WireGuard interface..."
    if command -v wg &> /dev/null; then
        ip link add wg0 type wireguard 2>/dev/null || true
    else
        # Use wireguard-go userspace implementation
        wireguard-go wg0 &
        sleep 2
    fi
fi

# Run as non-root if not requiring privileged operations
if [ "${REQUIRE_ROOT}" != "true" ]; then
    exec su-exec distributed /usr/local/bin/distributed "$@"
else
    exec /usr/local/bin/distributed "$@"
fi