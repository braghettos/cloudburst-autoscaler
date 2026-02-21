#!/bin/bash
# Shared UI deploy and verify for E2E tests.
# UI runs as a Docker container (like CloudBroker), uses kubeconfig to connect to the cluster.
# Source this file and call deploy_ui, verify_ui_ready. Call stop_ui from cleanup.
# Requires: PROJECT_ROOT, LOG_* functions (log_info, log_error), KIND_CLUSTER_NAME

UI_REPO="${PROJECT_ROOT}/../cloudburst-autoscaler-ui"
UI_IMAGE="cloudburst-autoscaler-ui:latest"
UI_CONTAINER="cloudburst-ui-e2e"
UI_KUBECONFIG_TMP=""

# stop_ui stops the UI container started by deploy_ui. Call from cleanup.
stop_ui() {
    if docker ps -a -q -f name="^${UI_CONTAINER}$" 2>/dev/null | grep -q .; then
        log_info "Stopping UI container ${UI_CONTAINER}..."
        docker stop "${UI_CONTAINER}" 2>/dev/null || true
        docker rm "${UI_CONTAINER}" 2>/dev/null || true
    fi
    if [ -n "${UI_KUBECONFIG_TMP:-}" ] && [ -f "${UI_KUBECONFIG_TMP}" ]; then
        rm -f "${UI_KUBECONFIG_TMP}"
    fi
}

deploy_ui() {
    if [ ! -d "${UI_REPO}" ]; then
        log_warn "cloudburst-autoscaler-ui not found at ${UI_REPO}, skipping UI deployment"
        return 0
    fi

    log_info "Starting CloudBurst UI as Docker container (like CloudBroker)..."

    # Build UI Docker image (from parent of both repos)
    local parent_dir
    parent_dir="$(cd "${PROJECT_ROOT}/.." && pwd)"
    log_info "Building UI Docker image: ${UI_IMAGE}..."
    if ! (cd "${parent_dir}" && docker build -f cloudburst-autoscaler-ui/Dockerfile -t "${UI_IMAGE}" . 2>&1 | tail -15); then
        log_error "Failed to build UI Docker image"
        return 1
    fi
    log_info "✓ UI Docker image built"

    # Create temp kubeconfig with host.docker.internal so container can reach Kind API
    UI_KUBECONFIG_TMP="/tmp/cloudburst-ui-kubeconfig-$$.config"
    export UI_KUBECONFIG_TMP

    # Ensure we export the Kind context (cluster ID = kind-${KIND_CLUSTER_NAME})
    local kind_ctx="kind-${KIND_CLUSTER_NAME:-cloudburst}"
    if ! kubectl config use-context "${kind_ctx}" 2>/dev/null; then
        log_error "Context ${kind_ctx} not found. Run: kubectl config get-contexts"
        return 1
    fi

    kubectl config view --minify --raw > "${UI_KUBECONFIG_TMP}" 2>/dev/null || {
        log_error "Failed to export kubeconfig"
        return 1
    }
    if ! grep -q "${kind_ctx}" "${UI_KUBECONFIG_TMP}" 2>/dev/null; then
        log_error "Exported kubeconfig missing context ${kind_ctx}"
        return 1
    fi
    chmod 644 "${UI_KUBECONFIG_TMP}" 2>/dev/null || true
    if [[ "$(uname -s)" == "Darwin" ]]; then
        sed -i '' 's|127\.0\.0\.1|host.docker.internal|g' "${UI_KUBECONFIG_TMP}" 2>/dev/null || true
    else
        sed -i 's|127\.0\.0\.1|host.docker.internal|g' "${UI_KUBECONFIG_TMP}" 2>/dev/null || true
    fi

    # Kind cert doesn't include host.docker.internal; skip TLS verify for E2E
    cluster_name="$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}' 2>/dev/null)"
    if [ -n "${cluster_name}" ]; then
        KUBECONFIG="${UI_KUBECONFIG_TMP}" kubectl config set-cluster "${cluster_name}" --insecure-skip-tls-verify=true 2>/dev/null || true
    fi

    # Stop any existing container
    docker stop "${UI_CONTAINER}" 2>/dev/null || true
    docker rm "${UI_CONTAINER}" 2>/dev/null || true

    log_info "Starting UI container on :8082..."
    local docker_args=(-d --name "${UI_CONTAINER}" -p 8082:8082
        -v "${UI_KUBECONFIG_TMP}:/kubeconfig:ro"
        -e KUBECONFIG=/kubeconfig)
    # Linux: host.docker.internal needs --add-host (Mac/Windows have it built-in)
    if [[ "$(uname -s)" == "Linux" ]]; then
        docker_args+=(--add-host=host.docker.internal:host-gateway)
    fi
    if ! docker run "${docker_args[@]}" "${UI_IMAGE}" 2>&1; then
        log_error "Failed to start UI container"
        rm -f "${UI_KUBECONFIG_TMP}"
        return 1
    fi
    sleep 3
    if ! docker ps -q -f name="^${UI_CONTAINER}$" | grep -q .; then
        log_error "UI container failed to start"
        docker logs "${UI_CONTAINER}" 2>&1 | tail -20
        rm -f "${UI_KUBECONFIG_TMP}"
        return 1
    fi
    log_info "✓ UI container running (${UI_CONTAINER})"
}

verify_ui_ready() {
    if [ ! -d "${UI_REPO}" ]; then
        return 0
    fi

    if ! docker ps -q -f name="^${UI_CONTAINER}$" 2>/dev/null | grep -q .; then
        log_warn "UI container not running, skipping verify"
        return 0
    fi

    # Verify UI and fleet API endpoints (container exposes 8082)
    if command -v curl &>/dev/null; then
        log_info "Verifying UI and API endpoints (http://127.0.0.1:8082)..."
        local base="http://127.0.0.1:8082"
        local failed=0

        # SPA and fleet list
        for path in "/" "/api/clusters"; do
            local code
            code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 "${base}${path}" 2>/dev/null || echo "000")
            if [ "$code" = "200" ]; then
                log_info "✓ ${path} -> 200"
            else
                log_error "${path} -> ${code} (expected 200)"
                failed=$((failed + 1))
            fi
        done

        # Scoped APIs: need cluster ID from /api/clusters
        local cluster_id
        cluster_id=$(curl -s --connect-timeout 5 "${base}/api/clusters" 2>/dev/null | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
        if [ -n "${cluster_id}" ]; then
            for path in "/api/clusters/${cluster_id}/dashboard" "/api/clusters/${cluster_id}/pods" "/api/clusters/${cluster_id}/nodeclaims" "/api/clusters/${cluster_id}/nodepools" "/api/clusters/${cluster_id}/nodes" "/api/clusters/${cluster_id}/graph" "/api/clusters/${cluster_id}/logs/controller"; do
                local code
                code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 "${base}${path}" 2>/dev/null || echo "000")
                if [ "$code" = "200" ]; then
                    log_info "✓ ${path} -> 200"
                else
                    log_error "${path} -> ${code} (expected 200)"
                    failed=$((failed + 1))
                fi
            done
        else
            log_warn "No cluster from /api/clusters, skipping scoped API checks"
        fi

        if [ $failed -gt 0 ]; then
            log_error "UI API verification failed: ${failed} endpoint(s) did not return 200"
            return 1
        fi
        log_info "✓ All UI API endpoints returned 200"
    fi
    return 0
}
