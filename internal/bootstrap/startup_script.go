/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bootstrap

import (
	"fmt"
	"regexp"
	"strings"
)

// GenerateStartupScript generates a cloud-init startup script for bootstrapping a node
func GenerateStartupScript(config *StartupScriptConfig) string {
	var script strings.Builder

	script.WriteString("#!/bin/bash\n")
	script.WriteString("set -euo pipefail\n\n")

	// Set node name (must match NodeClaim name so controller can find the node)
	script.WriteString(fmt.Sprintf("export NODE_NAME=%s\n", escapeShell(config.NodeName)))
	script.WriteString(fmt.Sprintf("export TAILSCALE_AUTHKEY=%s\n\n", escapeShell(config.TailscaleAuthKey)))

	// Set system hostname so kubelet registers the node with this name (controller expects node.Name == NodeClaim.Name)
	script.WriteString("# Set hostname so Kubernetes node name matches NodeClaim name\n")
	script.WriteString("hostnamectl set-hostname \"$NODE_NAME\" 2>/dev/null || true\n")
	script.WriteString("echo \"$NODE_NAME\" > /etc/hostname\n")
	script.WriteString("grep -q \"127.0.0.1 $NODE_NAME\" /etc/hosts || echo \"127.0.0.1 $NODE_NAME\" >> /etc/hosts\n\n")

	// Install Tailscale
	script.WriteString("# Install Tailscale\n")
	script.WriteString("curl -fsSL https://tailscale.com/install.sh | sh\n")
	script.WriteString("tailscale up --authkey=$TAILSCALE_AUTHKEY --hostname=$NODE_NAME --accept-routes --advertise-exit-node=false\n\n")

	// Wait for Tailscale to be ready and get IPv4
	script.WriteString("# Wait for Tailscale and get IPv4\n")
	script.WriteString("for i in {1..30}; do\n")
	script.WriteString("  TAILSCALE_IP=$(tailscale ip -4 2>/dev/null || echo \"\")\n")
	script.WriteString("  if [ -n \"$TAILSCALE_IP\" ]; then\n")
	script.WriteString("    break\n")
	script.WriteString("  fi\n")
	script.WriteString("  sleep 2\n")
	script.WriteString("done\n\n")

	script.WriteString("if [ -z \"$TAILSCALE_IP\" ]; then\n")
	script.WriteString("  echo \"ERROR: Failed to get Tailscale IP\"\n")
	script.WriteString("  exit 1\n")
	script.WriteString("fi\n\n")

	script.WriteString("echo \"Tailscale IP: $TAILSCALE_IP\"\n\n")

	// Stop, disable and mask kubelet until kubeadm join succeeds (avoids restart loop when bootstrap-kubelet.conf is missing)
	script.WriteString("systemctl stop kubelet 2>/dev/null || true\n")
	script.WriteString("systemctl disable kubelet 2>/dev/null || true\n")
	script.WriteString("systemctl mask kubelet 2>/dev/null || true\n\n")

	// Remove from Tailscale on shutdown so the device is removed from the tailnet when GCP deletes the instance
	script.WriteString("# Install Tailscale logout on shutdown (so VM is removed from tailnet when instance is deleted)\n")
	script.WriteString("cat > /etc/systemd/system/tailscale-logout-on-shutdown.service <<'SHUTDOWN'\n")
	script.WriteString("[Unit]\n")
	script.WriteString("Description=Remove this node from Tailscale on shutdown\n")
	script.WriteString("DefaultDependencies=no\n")
	script.WriteString("After=network.target\n")
	script.WriteString("Before=shutdown.target\n")
	script.WriteString("\n")
	script.WriteString("[Service]\n")
	script.WriteString("Type=oneshot\n")
	script.WriteString("RemainAfterExit=yes\n")
	script.WriteString("ExecStart=/bin/true\n")
	script.WriteString("ExecStop=-/usr/bin/tailscale logout\n")
	script.WriteString("TimeoutStopSec=15\n")
	script.WriteString("\n")
	script.WriteString("[Install]\n")
	script.WriteString("WantedBy=multi-user.target\n")
	script.WriteString("SHUTDOWN\n")
	script.WriteString("systemctl daemon-reload\n")
	script.WriteString("systemctl enable tailscale-logout-on-shutdown.service\n\n")

	// Disable swap
	script.WriteString("# Disable swap\n")
	script.WriteString("swapoff -a\n")
	script.WriteString("sed -i '/ swap / s/^/#/' /etc/fstab\n\n")

	// Install containerd
	script.WriteString("# Install containerd\n")
	script.WriteString("apt-get update\n")
	script.WriteString("apt-get install -y apt-transport-https ca-certificates curl jq\n")
	script.WriteString("mkdir -p /etc/apt/keyrings\n")
	script.WriteString("curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg\n")
	script.WriteString("echo \"deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable\" | tee /etc/apt/sources.list.d/docker.list > /dev/null\n")
	script.WriteString("apt-get update\n")
	script.WriteString("apt-get install -y containerd.io\n")
	script.WriteString("mkdir -p /etc/containerd\n")
	script.WriteString("containerd config default | tee /etc/containerd/config.toml > /dev/null\n")
	// For cgroup v2 + systemd, containerd must use SystemdCgroup = true so kubelet doesn't try to use a separate "kubelet" cgroup root.
	script.WriteString("sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml\n")
	script.WriteString("systemctl restart containerd\n")
	script.WriteString("systemctl enable containerd\n\n")

	// Install kubelet, kubeadm, kubectl
	k8sVersion := config.KubernetesVersion
	if k8sVersion == "" {
		k8sVersion = "1.34.3" // Default version
	}

	// Extract major.minor version for repository
	versionParts := strings.Split(k8sVersion, ".")
	if len(versionParts) < 2 {
		k8sVersion = "1.34.3"
		versionParts = []string{"1", "34"}
	}
	repoVersion := fmt.Sprintf("v%s.%s", versionParts[0], versionParts[1])

	script.WriteString(fmt.Sprintf("# Install kubelet, kubeadm, kubectl (version %s)\n", k8sVersion))
	script.WriteString(fmt.Sprintf("curl -fsSL https://pkgs.k8s.io/core:/stable:/%s/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg\n", repoVersion))
	script.WriteString(fmt.Sprintf("echo \"deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/%s/deb/ /\" | tee /etc/apt/sources.list.d/kubernetes.list\n", repoVersion))
	script.WriteString("apt-get update\n")
	script.WriteString(fmt.Sprintf("apt-get install -y kubelet=%s-1.1 kubeadm=%s-1.1 kubectl=%s-1.1\n", k8sVersion, k8sVersion, k8sVersion))
	script.WriteString("apt-mark hold kubelet kubeadm kubectl\n")
	script.WriteString("systemctl stop kubelet 2>/dev/null || true\n")
	script.WriteString("systemctl disable kubelet 2>/dev/null || true\n")
	script.WriteString("systemctl mask kubelet 2>/dev/null || true\n\n")

	// Set kubelet node-ip to Tailscale IPv4 and cgroup driver for cgroups v2 (Ubuntu / GCP images)
	script.WriteString("# Configure kubelet: Tailscale node-ip and systemd cgroup driver\n")
	script.WriteString("mkdir -p /etc/systemd/system/kubelet.service.d\n")
	script.WriteString("cat > /etc/systemd/system/kubelet.service.d/10-node-ip.conf <<EOF\n")
	script.WriteString("[Service]\n")
	script.WriteString("Environment=\"KUBELET_EXTRA_ARGS=--node-ip=$TAILSCALE_IP --cgroup-driver=systemd --register-with-taints=cloudburst.io/not-ready=true:NoSchedule\"\n")
	script.WriteString("EOF\n")
	script.WriteString("systemctl daemon-reload\n\n")

	// kubeadm preflight: bridge-nf-call-iptables and ip_forward (required for worker nodes)
	script.WriteString("# kubeadm preflight: enable bridge and ip_forward\n")
	script.WriteString("modprobe br_netfilter\n")
	script.WriteString("echo 1 > /proc/sys/net/bridge/bridge-nf-call-iptables\n")
	script.WriteString("echo 1 > /proc/sys/net/ipv4/ip_forward\n")
	script.WriteString("cat > /etc/sysctl.d/99-kubernetes.conf <<'SYSCTLEOF'\n")
	script.WriteString("net.bridge.bridge-nf-call-iptables = 1\n")
	script.WriteString("net.ipv4.ip_forward = 1\n")
	script.WriteString("SYSCTLEOF\n")
	script.WriteString("sysctl --system >/dev/null 2>&1 || true\n\n")

	// Wait for containerd to be ready (kubelet depends on it)
	script.WriteString("# Wait for containerd to be ready before kubeadm join\n")
	script.WriteString("for i in 1 2 3 4 5 6 7 8 9 10; do\n")
	script.WriteString("  if ctr version >/dev/null 2>&1; then break; fi\n")
	script.WriteString("  sleep 3\n")
	script.WriteString("done\n")
	script.WriteString("ctr version >/dev/null 2>&1 || { echo \"ERROR: containerd not ready\"; exit 1; }\n\n")

	// Execute kubeadm join with custom API server endpoint
	script.WriteString("# Join cluster with custom API server endpoint\n")
	script.WriteString(fmt.Sprintf("export KUBEADM_JOIN_CMD=%q\n", escapeShell(config.JoinCommand)))
	// Full URL for curl health check
	script.WriteString(fmt.Sprintf("export CUSTOM_API_SERVER=%s\n", escapeShell(config.HostApiServer)))
	// kubeadm join expects host:port only (computed in Go to avoid shell quoting issues)
	script.WriteString(fmt.Sprintf("export JOIN_ENDPOINT=%s\n", escapeShell(hostPortFromURL(config.HostApiServer))))
	script.WriteString("# Extract token and CA hash from join command (--token VALUE or --token=VALUE; trim quotes)\n")
	script.WriteString("JOIN_TOKEN=$(echo \"$KUBEADM_JOIN_CMD\" | sed -n 's/.*--token[= ]\\([^ ]*\\).*/\\1/p' | sed \"s/^['\\\"]*//;s/['\\\"]*$//\")\n")
	script.WriteString("CA_HASH=$(echo \"$KUBEADM_JOIN_CMD\" | sed -n 's/.*--discovery-token-ca-cert-hash \\([^ ]*\\).*/\\1/p' | sed \"s/^['\\\"]*//;s/['\\\"]*$//\")\n")
	script.WriteString("# Resolve API server hostname to Tailscale IP and DNS name (host or peer).\n")
	script.WriteString("API_HOST=$(printf '%s' \"${JOIN_ENDPOINT%%:*}\" | tr -d '\\n\\r\\t ')\n")
	script.WriteString("API_SERVER_PORT=\"${JOIN_ENDPOINT#*:}\"\n")
	script.WriteString("[ -z \"$API_HOST\" ] && { echo \"ERROR: API_HOST is empty (JOIN_ENDPOINT=$JOIN_ENDPOINT)\"; exit 1; }\n")
	script.WriteString("OPERATOR_IP=\"\"\n")
	script.WriteString("OPERATOR_DNS_NAME=\"\"\n")
	// Match peer by: HostName equals/starts-with API_HOST, or API_HOST starts with HostName (e.g. MagicDNS FQDN), or DNSName equals/starts-with API_HOST.
	script.WriteString("for i in $(seq 1 30); do\n")
	script.WriteString("  JSON=$(tailscale status --json 2>/dev/null)\n")
	script.WriteString("  OPERATOR_IP=$(echo \"$JSON\" | jq -r --arg h \"$API_HOST\" '(.Peer // {}) | to_entries[] | .value | select(.HostName == $h or (.HostName | startswith($h)) or ($h | startswith(.HostName)) or (.DNSName != null and ((.DNSName | gsub(\"[.]$\"; \"\")) == $h or ((.DNSName | gsub(\"[.]$\"; \"\")) | startswith($h)))) | .TailscaleIPs[0]' 2>/dev/null | head -1) || true\n")
	script.WriteString("  OPERATOR_DNS_NAME=$(echo \"$JSON\" | jq -r --arg h \"$API_HOST\" '(.Peer // {}) | to_entries[] | .value | select(.HostName == $h or (.HostName | startswith($h)) or ($h | startswith(.HostName)) or (.DNSName != null and ((.DNSName | gsub(\"[.]$\"; \"\")) == $h or ((.DNSName | gsub(\"[.]$\"; \"\")) | startswith($h)))) | .DNSName' 2>/dev/null | head -1 | sed 's/\\.$//') || true\n")
	script.WriteString("  if [ -n \"$OPERATOR_IP\" ] && [ \"$OPERATOR_IP\" != \"null\" ]; then\n")
	script.WriteString("    echo \"$OPERATOR_IP $API_HOST\" >> /etc/hosts\n")
	script.WriteString("    [ -n \"$OPERATOR_DNS_NAME\" ] && [ \"$OPERATOR_DNS_NAME\" != \"null\" ] && echo \"$OPERATOR_IP $OPERATOR_DNS_NAME\" >> /etc/hosts\n")
	script.WriteString("    echo \"Resolved $API_HOST to $OPERATOR_IP\"\n")
	script.WriteString("    break\n")
	script.WriteString("  fi\n")
	script.WriteString("  sleep 2\n")
	script.WriteString("done\n")
	script.WriteString("# If tailscale status did not list a peer matching API_HOST, try MagicDNS resolution.\n")
	script.WriteString("if [ -z \"$OPERATOR_IP\" ] || [ \"$OPERATOR_IP\" = \"null\" ]; then\n")
	script.WriteString("  OPERATOR_IP=$(getent ahostsv4 \"$API_HOST\" 2>/dev/null | awk '{print $1; exit}') || true\n")
	script.WriteString("  if [ -n \"$OPERATOR_IP\" ]; then\n")
	script.WriteString("    echo \"$OPERATOR_IP $API_HOST\" >> /etc/hosts\n")
	script.WriteString("    echo \"Resolved $API_HOST to $OPERATOR_IP (via MagicDNS getent)\"\n")
	script.WriteString("  else\n")
	script.WriteString("    echo \"ERROR: Could not resolve $API_HOST from tailscale status or MagicDNS (is the host on the tailnet?)\"\n")
	script.WriteString("    exit 1\n")
	script.WriteString("  fi\n")
	script.WriteString("fi\n")
	script.WriteString("# Use API host (IP or MagicDNS). Sanitize to avoid curl exit 3 (URL malformed).\n")
	script.WriteString("JOIN_HOST=\"$API_HOST\"\n")
	script.WriteString("[ -n \"$OPERATOR_DNS_NAME\" ] && [ \"$OPERATOR_DNS_NAME\" != \"null\" ] && JOIN_HOST=\"$OPERATOR_DNS_NAME\"\n")
	script.WriteString("JOIN_HOST=$(printf '%s' \"$JOIN_HOST\" | tr -d '\\n\\r\\t ')\n")
	script.WriteString("[ -z \"$JOIN_HOST\" ] && { echo \"ERROR: JOIN_HOST is empty\"; exit 1; }\n")
	script.WriteString("OPERATOR_IP=$(printf '%s' \"$OPERATOR_IP\" | tr -d '\\n\\r\\t ')\n")
	if config.DiscoveryKubeconfigBase64 != "" {
		script.WriteString("# Use embedded discovery kubeconfig (controller set server to hostApiServer; VM does not need to fetch cluster-info)\n")
		script.WriteString(fmt.Sprintf("echo %s | base64 -d > /tmp/discovery-kubeconfig.yaml\n", escapeShell(config.DiscoveryKubeconfigBase64)))
	} else {
		script.WriteString("# Fetch cluster-info from API server (host or proxy). Use JOIN_ENDPOINT so port is correct (e.g. 6443 for kind on host).\n")
		script.WriteString("[ -z \"$OPERATOR_IP\" ] && { echo \"ERROR: OPERATOR_IP is empty (cannot resolve for cluster-info fetch)\"; exit 1; }\n")
		script.WriteString("DISCOVERY_URL=\"https://${JOIN_ENDPOINT}/api/v1/namespaces/kube-public/configmaps/cluster-info\"\n")
		script.WriteString("CURL_RESOLVE=\"--resolve ${API_HOST}:${API_SERVER_PORT}:${OPERATOR_IP}\"\n")
		script.WriteString("echo \"cluster-info fetch: JOIN_ENDPOINT='$JOIN_ENDPOINT' OPERATOR_IP='$OPERATOR_IP'\"\n")
		script.WriteString("for i in 1 2 3 4 5; do\n")
		script.WriteString("  FETCHED=0\n")
		script.WriteString("  for tls_opt in \"--tlsv1.2\" \"--tlsv1.3\" \"\"; do\n")
		script.WriteString("    if ( curl -k $tls_opt $CURL_RESOLVE -sSf \"$DISCOVERY_URL\" -o /tmp/cluster-info.json 2>/dev/null ); then\n")
		script.WriteString("      FETCHED=1\n")
		script.WriteString("      break\n")
		script.WriteString("    fi\n")
		script.WriteString("  done\n")
		script.WriteString("  if [ \"$FETCHED\" = \"1\" ]; then\n")
		script.WriteString("    KC=$(jq -r '.data.kubeconfig // .data | to_entries[0].value' /tmp/cluster-info.json 2>/dev/null)\n")
		script.WriteString("    if [ -n \"$KC\" ] && [ \"$KC\" != \"null\" ]; then\n")
		script.WriteString("      echo \"$KC\" | sed \"s|server: .*|server: https://${JOIN_ENDPOINT}|\" > /tmp/discovery-kubeconfig.yaml\n")
		script.WriteString("      # When API server is on 6443 (host), keep cluster CA. When on 443 (TLS proxy), strip CA and add insecure-skip-tls-verify.\n")
		script.WriteString("      if [ \"$API_SERVER_PORT\" != \"6443\" ]; then\n")
		script.WriteString("        sed -i '/certificate-authority-data:/d;/certificate-authority:/d' /tmp/discovery-kubeconfig.yaml\n")
		script.WriteString("        sed -i '/cluster:/a\\      insecure-skip-tls-verify: true' /tmp/discovery-kubeconfig.yaml\n")
		script.WriteString("      fi\n")
		script.WriteString("      break\n")
		script.WriteString("    fi\n")
		script.WriteString("  fi\n")
		script.WriteString("  if [ \"$i\" = \"5\" ]; then\n")
		script.WriteString("    echo \"ERROR: Failed to fetch cluster-info from $DISCOVERY_URL (last curl error below)\"\n")
		script.WriteString("    curl -k --tlsv1.2 $CURL_RESOLVE -sSf \"$DISCOVERY_URL\" -o /tmp/cluster-info.json 2>&1 || true\n")
		script.WriteString("    exit 1\n")
		script.WriteString("  fi\n")
		script.WriteString("  sleep 5\n")
		script.WriteString("done\n")
	}
	// Use kubeadm join with embedded discovery kubeconfig. With Tailscale on host, server is host:6443 and cluster CA is used.
	if config.DiscoveryKubeconfigBase64 != "" {
		script.WriteString("# Join cluster with kubeadm using embedded discovery kubeconfig (server = hostApiServer, cluster CA when host:6443).\n")
		script.WriteString("echo \"Discovery kubeconfig server: $(grep -E '^\\s*server:' /tmp/discovery-kubeconfig.yaml 2>/dev/null || echo 'unknown')\"\n")
		script.WriteString("# Wait for connectivity to API server (host Tailscale IP:6443) before kubeadm join\n")
		script.WriteString("echo \"Waiting for API server at $JOIN_ENDPOINT (up to 5 minutes)...\"\n")
		script.WriteString("for i in $(seq 1 60); do\n")
		script.WriteString("  if curl -k -sSf --connect-timeout 5 \"https://${JOIN_ENDPOINT}/healthz\" >/dev/null 2>&1; then\n")
		script.WriteString("    echo \"API server reachable after ${i} attempts\"\n")
		script.WriteString("    break\n")
		script.WriteString("  fi\n")
		script.WriteString("  [ $((i % 10)) -eq 0 ] && echo \"Waiting for API server connectivity (attempt $i/60)...\"\n")
		script.WriteString("  [ \"$i\" = \"60\" ] && { echo \"ERROR: Cannot reach API server after 60 attempts. Is Tailscale connected?\"; exit 1; }\n")
		script.WriteString("  sleep 5\n")
		script.WriteString("done\n")
		script.WriteString("# Re-apply hostname so Kubernetes node name matches NodeClaim (GCP/guest agent may have overwritten it).\n")
		script.WriteString("hostnamectl set-hostname \"$NODE_NAME\" 2>/dev/null || true\n")
		script.WriteString("echo \"$NODE_NAME\" > /etc/hostname\n")
		script.WriteString("# Stop and mask kubelet so it does not restart after kubeadm reset.\n")
		script.WriteString("systemctl stop kubelet 2>/dev/null || true\n")
		script.WriteString("systemctl disable kubelet 2>/dev/null || true\n")
		script.WriteString("systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("KUBEADM_JOIN=\"kubeadm join --discovery-file=/tmp/discovery-kubeconfig.yaml --skip-phases=preflight --v=2\"\n")
		script.WriteString("[ -n \"$JOIN_TOKEN\" ] && KUBEADM_JOIN=\"$KUBEADM_JOIN --token $JOIN_TOKEN\"\n")
		script.WriteString("[ -z \"$JOIN_TOKEN\" ] && echo \"WARN: No bootstrap token from join command; kubeadm may fail with 'couldn't find authentication credentials'\"\n")
		script.WriteString("for attempt in 1 2 3; do\n")
		script.WriteString("  kubeadm reset -f 2>/dev/null || true\n")
		// Ensure we don't reuse a half-written client config on retries.
		script.WriteString("  rm -f /etc/kubernetes/kubelet.conf /var/lib/kubelet/pki/kubelet-client-* 2>/dev/null || true\n")
		script.WriteString("  systemctl stop kubelet 2>/dev/null || true\n")
		script.WriteString("  systemctl unmask kubelet 2>/dev/null || true\n")
		// Background watcher: as soon as kubeadm writes /var/lib/kubelet/config.yaml, strip cgroupRoot
		// so kubelet (started by kubeadm during join) doesn't crash on cgroup-v2 VMs.
		script.WriteString("  ( while true; do if [ -f /var/lib/kubelet/config.yaml ]; then sed -i '/cgroupRoot:/d' /var/lib/kubelet/config.yaml; break; fi; sleep 0.5; done ) &\n")
		script.WriteString("  CGROUP_WATCHER_PID=$!\n")
		script.WriteString("  echo \"Join attempt $attempt\"\n")
		script.WriteString("  if $KUBEADM_JOIN 2>&1; then\n")
		script.WriteString("    echo \"kubeadm join succeeded on attempt $attempt\"\n")
		script.WriteString("  else\n")
		script.WriteString("    KUBEADM_EXIT=$?\n")
		script.WriteString("    echo \"Join failed on attempt $attempt (exit code $KUBEADM_EXIT)\"\n")
		script.WriteString("    kill $CGROUP_WATCHER_PID 2>/dev/null || true; wait $CGROUP_WATCHER_PID 2>/dev/null || true\n")
		script.WriteString("    systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("    if [ \"$attempt\" = \"3\" ]; then echo \"ERROR: Bootstrap failed after 3 join attempts\"; exit 1; fi\n")
		script.WriteString("    sleep 30\n")
		script.WriteString("    continue\n")
		script.WriteString("  fi\n")
		script.WriteString("  kill $CGROUP_WATCHER_PID 2>/dev/null || true; wait $CGROUP_WATCHER_PID 2>/dev/null || true\n")
		script.WriteString("  # When API server is via a TLS proxy (e.g. hostname contains 'tailscale'), kubelet may need insecure-skip-tls-verify. Patch if needed.\n")
		script.WriteString("  if echo \"$CUSTOM_API_SERVER\" | grep -qi tailscale; then\n")
		script.WriteString("    grep -q 'insecure-skip-tls-verify' /etc/kubernetes/bootstrap-kubelet.conf 2>/dev/null || \\\n")
		script.WriteString("      sed -i '/cluster:/a\\    insecure-skip-tls-verify: true' /etc/kubernetes/bootstrap-kubelet.conf 2>/dev/null || true\n")
		script.WriteString("  fi\n")
		// Strip cgroupRoot again (belt-and-suspenders, in case watcher missed a race)
		script.WriteString("  if [ -f /var/lib/kubelet/config.yaml ]; then sed -i '/cgroupRoot:/d' /var/lib/kubelet/config.yaml || true; fi\n")
		script.WriteString("  systemctl enable kubelet 2>/dev/null || true\n")
		script.WriteString("  systemctl restart kubelet\n")
		script.WriteString("  echo \"Waiting for kubelet healthz (up to 48*10s)\"\n")
		script.WriteString("  for i in $(seq 1 48); do\n")
		script.WriteString("    if curl -sSf http://127.0.0.1:10248/healthz >/dev/null 2>&1; then echo \"Kubelet healthy after ${i} checks\"; break; fi\n")
		script.WriteString("    [ $((i % 6)) -eq 0 ] && echo \"kubelet healthz wait attempt $i/48\"\n")
		script.WriteString("    [ \"$i\" = \"48\" ] && { echo \"kubelet never became healthy (last 30 lines kubelet journal):\"; journalctl -u kubelet -n 30 --no-pager 2>/dev/null || true; [ \"$attempt\" = \"3\" ] && exit 1; break; }\n")
		script.WriteString("    sleep 10\n")
		script.WriteString("  done\n")
		script.WriteString("  if curl -sSf http://127.0.0.1:10248/healthz >/dev/null 2>&1; then break; fi\n")
		script.WriteString("  echo \"Kubelet not healthy this attempt, retrying join loop\"\n")
		script.WriteString("  systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("  sleep 30\n")
		script.WriteString("done\n")
	} else {
		script.WriteString("# Re-apply hostname so Kubernetes node name matches NodeClaim (GCP/guest agent may have overwritten it).\n")
		script.WriteString("hostnamectl set-hostname \"$NODE_NAME\" 2>/dev/null || true\n")
		script.WriteString("echo \"$NODE_NAME\" > /etc/hostname\n")
		script.WriteString("systemctl stop kubelet 2>/dev/null || true\n")
		script.WriteString("systemctl disable kubelet 2>/dev/null || true\n")
		script.WriteString("systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("# Join cluster with kubeadm (discovery fetched from API server).\n")
		script.WriteString("echo \"Discovery kubeconfig server: $(grep -E '^\\s*server:' /tmp/discovery-kubeconfig.yaml 2>/dev/null || echo 'unknown')\"\n")
		script.WriteString("KUBEADM_JOIN=\"kubeadm join --discovery-file=/tmp/discovery-kubeconfig.yaml --skip-phases=preflight --v=2\"\n")
		script.WriteString("[ -n \"$JOIN_TOKEN\" ] && KUBEADM_JOIN=\"$KUBEADM_JOIN --token $JOIN_TOKEN\"\n")
		script.WriteString("[ -z \"$JOIN_TOKEN\" ] && echo \"WARN: No bootstrap token from join command\"\n")
		script.WriteString("for attempt in 1 2 3; do\n")
		script.WriteString("  kubeadm reset -f 2>/dev/null || true\n")
		// Ensure we don't reuse a half-written client config on retries.
		script.WriteString("  rm -f /etc/kubernetes/kubelet.conf /var/lib/kubelet/pki/kubelet-client-* 2>/dev/null || true\n")
		script.WriteString("  systemctl stop kubelet 2>/dev/null || true\n")
		script.WriteString("  systemctl unmask kubelet 2>/dev/null || true\n")
		// Background watcher: strip cgroupRoot as soon as kubeadm writes config.yaml (before kubelet starts)
		script.WriteString("  ( while true; do if [ -f /var/lib/kubelet/config.yaml ]; then sed -i '/cgroupRoot:/d' /var/lib/kubelet/config.yaml; break; fi; sleep 0.5; done ) &\n")
		script.WriteString("  CGROUP_WATCHER_PID=$!\n")
		script.WriteString("  echo \"Join attempt $attempt\"\n")
		script.WriteString("  if $KUBEADM_JOIN 2>&1; then\n")
		script.WriteString("    echo \"kubeadm join succeeded on attempt $attempt\"\n")
		script.WriteString("  else\n")
		script.WriteString("    KUBEADM_EXIT=$?\n")
		script.WriteString("    echo \"Join failed on attempt $attempt (exit code $KUBEADM_EXIT)\"\n")
		script.WriteString("    kill $CGROUP_WATCHER_PID 2>/dev/null || true; wait $CGROUP_WATCHER_PID 2>/dev/null || true\n")
		script.WriteString("    if [ \"$attempt\" = \"3\" ]; then echo \"ERROR: Bootstrap failed after 3 join attempts\"; exit 1; fi\n")
		script.WriteString("    systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("    sleep 30\n")
		script.WriteString("    continue\n")
		script.WriteString("  fi\n")
		script.WriteString("  kill $CGROUP_WATCHER_PID 2>/dev/null || true; wait $CGROUP_WATCHER_PID 2>/dev/null || true\n")
		script.WriteString("  if echo \"$CUSTOM_API_SERVER\" | grep -qi tailscale; then\n")
		script.WriteString("    grep -q 'insecure-skip-tls-verify' /etc/kubernetes/bootstrap-kubelet.conf 2>/dev/null || \\\n")
		script.WriteString("      sed -i '/cluster:/a\\    insecure-skip-tls-verify: true' /etc/kubernetes/bootstrap-kubelet.conf 2>/dev/null || true\n")
		script.WriteString("  fi\n")
		// Belt-and-suspenders: strip cgroupRoot again after join completes
		script.WriteString("  if [ -f /var/lib/kubelet/config.yaml ]; then sed -i '/cgroupRoot:/d' /var/lib/kubelet/config.yaml || true; fi\n")
		script.WriteString("  systemctl enable kubelet 2>/dev/null || true\n")
		script.WriteString("  systemctl restart kubelet\n")
		script.WriteString("  echo \"Waiting for kubelet healthz (up to 48*10s)\"\n")
		script.WriteString("  for i in $(seq 1 48); do\n")
		script.WriteString("    if curl -sSf http://127.0.0.1:10248/healthz >/dev/null 2>&1; then echo \"Kubelet healthy after ${i} checks\"; break; fi\n")
		script.WriteString("    [ $((i % 6)) -eq 0 ] && echo \"kubelet healthz wait attempt $i/48\"\n")
		script.WriteString("    [ \"$i\" = \"48\" ] && { echo \"kubelet never became healthy\"; journalctl -u kubelet -n 30 --no-pager 2>/dev/null || true; [ \"$attempt\" = \"3\" ] && exit 1; break; }\n")
		script.WriteString("    sleep 10\n")
		script.WriteString("  done\n")
		script.WriteString("  if curl -sSf http://127.0.0.1:10248/healthz >/dev/null 2>&1; then break; fi\n")
		script.WriteString("  echo \"Kubelet not healthy this attempt, retrying join loop\"\n")
		script.WriteString("  systemctl mask kubelet 2>/dev/null || true\n")
		script.WriteString("  sleep 30\n")
		script.WriteString("done\n")
	}
	script.WriteString("\n")

	// Fallback CNI config: if kindnet (or any other DaemonSet CNI) hasn't written a CNI config
	// within 30 seconds, write a minimal bridge CNI config using the node's assigned podCIDR.
	// This is needed for external nodes (e.g. GCP VMs joining a kind cluster) where kindnet
	// may not work because it expects kind's internal Docker network.
	script.WriteString("# Fallback CNI: ensure /etc/cni/net.d/ has a valid config so node becomes Ready.\n")
	script.WriteString("echo \"Checking if CNI config exists...\"\n")
	script.WriteString("CNI_READY=false\n")
	script.WriteString("for i in $(seq 1 6); do\n")
	script.WriteString("  if ls /etc/cni/net.d/*.conflist /etc/cni/net.d/*.conf 2>/dev/null | grep -qv 'kubernetes-cni-keep'; then\n")
	script.WriteString("    echo \"CNI config found after $((i*5))s\"\n")
	script.WriteString("    CNI_READY=true\n")
	script.WriteString("    break\n")
	script.WriteString("  fi\n")
	script.WriteString("  sleep 5\n")
	script.WriteString("done\n")
	script.WriteString("if [ \"$CNI_READY\" = \"false\" ]; then\n")
	script.WriteString("  echo \"No CNI config found after 30s, writing fallback bridge CNI config...\"\n")
	script.WriteString("  # Get podCIDR assigned to this node by the control plane\n")
	script.WriteString("  POD_CIDR=\"\"\n")
	script.WriteString("  for i in $(seq 1 12); do\n")
	script.WriteString("    POD_CIDR=$(KUBECONFIG=/etc/kubernetes/kubelet.conf kubectl get node \"$(hostname)\" -o jsonpath='{.spec.podCIDR}' 2>/dev/null || true)\n")
	script.WriteString("    if [ -n \"$POD_CIDR\" ]; then echo \"Node podCIDR: $POD_CIDR\"; break; fi\n")
	script.WriteString("    echo \"Waiting for podCIDR assignment ($i/12)...\"\n")
	script.WriteString("    sleep 5\n")
	script.WriteString("  done\n")
	script.WriteString("  if [ -z \"$POD_CIDR\" ]; then POD_CIDR=\"10.244.1.0/24\"; echo \"WARN: podCIDR not assigned, using default $POD_CIDR\"; fi\n")
	script.WriteString("  mkdir -p /etc/cni/net.d\n")
	script.WriteString("  cat > /etc/cni/net.d/10-bridge.conflist <<CNIEOF\n")
	script.WriteString("{\n")
	script.WriteString("  \"cniVersion\": \"0.3.1\",\n")
	script.WriteString("  \"name\": \"bridge\",\n")
	script.WriteString("  \"plugins\": [\n")
	script.WriteString("    {\n")
	script.WriteString("      \"type\": \"bridge\",\n")
	script.WriteString("      \"bridge\": \"cni0\",\n")
	script.WriteString("      \"isGateway\": true,\n")
	script.WriteString("      \"ipMasq\": true,\n")
	script.WriteString("      \"ipam\": {\n")
	script.WriteString("        \"type\": \"host-local\",\n")
	script.WriteString("        \"ranges\": [[{\"subnet\": \"$POD_CIDR\"}]],\n")
	script.WriteString("        \"routes\": [{\"dst\": \"0.0.0.0/0\"}]\n")
	script.WriteString("      }\n")
	script.WriteString("    },\n")
	script.WriteString("    {\n")
	script.WriteString("      \"type\": \"portmap\",\n")
	script.WriteString("      \"capabilities\": {\"portMappings\": true}\n")
	script.WriteString("    }\n")
	script.WriteString("  ]\n")
	script.WriteString("}\n")
	script.WriteString("CNIEOF\n")
	script.WriteString("  echo \"Fallback CNI bridge config written to /etc/cni/net.d/10-bridge.conflist\"\n")
	script.WriteString("  # Restart kubelet to pick up the new CNI config\n")
	script.WriteString("  systemctl restart kubelet\n")
	script.WriteString("  sleep 5\n")
	script.WriteString("fi\n")
	script.WriteString("\n")

	script.WriteString("echo \"Node bootstrap completed successfully\"\n")

	return script.String()
}

// StartupScriptConfig contains configuration for generating startup script
type StartupScriptConfig struct {
	NodeName          string
	TailscaleAuthKey  string
	KubernetesVersion string
	JoinCommand       string
	HostApiServer     string
	// DiscoveryKubeconfigBase64 is optional: base64-encoded discovery kubeconfig (cluster-info) with server URL set to HostApiServer. If set, the VM skips fetching cluster-info over the network.
	DiscoveryKubeconfigBase64 string
}

// ReplaceDiscoveryKubeconfigServer replaces the cluster server URL in a kubeconfig (e.g. from cluster-info ConfigMap) with the given serverURL.
func ReplaceDiscoveryKubeconfigServer(kubeconfigContent, serverURL string) string {
	re := regexp.MustCompile(`(?m)^(\s*server:\s*).*$`)
	return re.ReplaceAllString(kubeconfigContent, "${1}"+serverURL)
}

// PrepareDiscoveryKubeconfigForProxy replaces the cluster server URL, removes cluster CA, and sets insecure-skip-tls-verify: true in every cluster block.
// Use when the API server is reached via a TLS-terminating proxy that presents its own cert (not the cluster CA).
// Kubeadm does not allow both certificate-authority and insecure-skip-tls-verify, so we remove the CA from each cluster block.
func PrepareDiscoveryKubeconfigForProxy(kubeconfigContent, serverURL string) string {
	// Replace server URL in all cluster blocks. Quote the URL so YAML parsers don't treat colons in "https://host:443" as key-value.
	re := regexp.MustCompile(`(?m)^(\s*)(server:\s*)(.*)$`)
	withServer := re.ReplaceAllString(kubeconfigContent, "${1}$2\""+serverURL+"\"")
	lines := strings.Split(withServer, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "cluster:" && trimmed != "- cluster:" {
			continue
		}
		clusterIndent := leadingSpaces(lines[i])
		if i+1 >= len(lines) {
			continue
		}
		contentIndent := leadingSpaces(lines[i+1])
		if contentIndent == "" {
			contentIndent = "    "
		}
		// Remove certificate-authority-data and certificate-authority from this cluster block (kubeadm forbids them with insecure-skip-tls-verify).
		j := i + 1
		for j < len(lines) && leadingSpaces(lines[j]) > clusterIndent {
			trimmedLine := strings.TrimSpace(lines[j])
			if strings.HasPrefix(trimmedLine, "certificate-authority-data:") || strings.HasPrefix(trimmedLine, "certificate-authority:") {
				lines = append(lines[:j], lines[j+1:]...)
				continue
			}
			j++
		}
		// Insert insecure-skip-tls-verify after the "cluster:" line (still at index i).
		insert := contentIndent + "insecure-skip-tls-verify: true"
		if i+1 <= len(lines) {
			lines = append(lines[:i+1], append([]string{insert}, lines[i+1:]...)...)
		}
		i++ // skip the line we just inserted so we don't match it as another "cluster:"
	}
	return strings.Join(lines, "\n")
}

func leadingSpaces(s string) string {
	var n string
	for _, c := range s {
		if c == ' ' || c == '\t' {
			n += string(c)
		} else {
			break
		}
	}
	return n
}

// hostPortFromURL returns "host:port" from a URL like "https://1.2.3.4:6443" (kubeadm join expects this format).
func hostPortFromURL(url string) string {
	s := url
	if idx := strings.Index(s, "://"); idx != -1 {
		s = s[idx+3:]
	}
	return s
}

// escapeShell escapes shell special characters
func escapeShell(s string) string {
	// Replace single quotes with '\''
	s = strings.ReplaceAll(s, "'", "'\\''")
	return "'" + s + "'"
}
