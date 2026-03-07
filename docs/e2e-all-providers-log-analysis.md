# E2E All-Providers Controller Log Analysis

**Log file:** `controller-e2e-all-providers.log` (3764 lines)  
**Run:** 2026-03-04 17:31–17:47 UTC

## Summary

- **Concurrency / Graph:** No issues. Each of 8 NodePools created exactly 1 NodeClaim. No duplicate creates, no "at maxNodes" or "would exceed maxNodes" messages.
- **NodePool controller:** Behaving correctly under concurrent load.
- **NodeClaim controller:** Several provider-specific errors during create and delete.

---

## Issues Found

### 1. Azure VM creation failed (ERROR)

```
failed to create instance ... Standard_D2pls_v5
RESPONSE 400: Bad Request - "Cannot create a VM of size 'Standard_D2pls_v5' because this VM size only supports..."
```

**Cause:** CloudBroker recommended `Standard_D2pls_v5`, but this size is not available in the target region/subscription (e.g. ARM-only, or region restriction).

**Impact:** Azure NodeClaim never provisions; E2E can fail if Azure is required.

---

### 2. 404 on instance delete treated as ERROR (GCP, DigitalOcean, OVH)

```
failed to delete instance: googleapi: Error 404: The resource '...' was not found
failed to delete droplet: ... 404 The resource you were accessing could not be found
OVHcloud API error (status code 404): "not found: instance ... not found"
```

**Cause:** Instance was already deleted (e.g. by `cleanup-all-providers.sh`, manual delete, or parallel run). Controller treats 404 as a hard error.

**Impact:** NodeClaim stays in Deleting, finalizer blocks CR deletion, scale-down can hang.

---

### 3. Aruba "CloudServer cannot be deleted" (400)

```
Aruba delete error 400: {"fieldName":"State","errorMessage":"CloudServer cannot be deleted"}
```

**Cause:** Aruba API rejects delete while the server is in a state that does not allow deletion (e.g. running, provisioning).

**Impact:** NodeClaim stuck in Deleting until server state allows delete.

---

## Fix Plan

### Fix 1: Treat 404 on delete as success (idempotent delete)

**File:** `internal/controller/nodeclaim_controller.go` (or provider-specific delete logic)

**Change:** When DeleteInstance returns 404 (Not Found), treat it as success: instance is already gone. Proceed to remove finalizer and delete the NodeClaim.

**Implementation:**
- In `handleDeletion`, after calling provider Delete:
  - If error contains "404" or "not found" or `IsNotFound`-style check: treat as success, clear finalizer, delete NodeClaim.
  - Else: return error for retry.

**Providers to update:** GCP, DigitalOcean, OVH, Scaleway, Hetzner, AWS, Azure, Aruba (each may return 404 differently).

---

### Fix 2: Azure VM size / region compatibility

**Options:**

1. **CloudBroker:** Filter out instance types that are not available in the target region (e.g. ARM-only sizes when region only has x86).
2. **NodeClaim controller:** On 400 BadRequest with "Cannot create a VM of size", mark NodeClaim as failed (e.g. `ProvisioningFailed`) and optionally trigger a retry with the next CloudBroker alternative.
3. **NodeClass / E2E:** Restrict Azure regions or instance types in E2E to known-compatible ones.

**Recommendation:** Start with (2): detect this specific Azure error, set phase to `ProvisioningFailed` or similar, and log clearly. Then (1) in CloudBroker for a proper fix.

---

### Fix 3: Aruba delete – handle "cannot be deleted" state

**Options:**

1. **Power off first:** Call Aruba power-off before delete when state disallows delete.
2. **Retry with backoff:** Requeue with `RequeueAfter` (e.g. 30s) when 400 "cannot be deleted" is returned.
3. **Check state:** Query server state before delete; if not deletable, power off and wait, then retry delete.

**Recommendation:** (2) is simplest: retry with backoff. (1) is more robust if Aruba API supports power-off.

---

## Implemented Fixes (2026-03-04)

### Fix 1: Treat 404 on delete as success ✅

**File:** `internal/controller/nodeclaim_controller.go`

- Added `isInstanceNotFoundError(err)` helper that detects 404/not found in error message.
- When `DeleteInstance` returns such an error: log as INFO "instance already deleted", record Normal event, proceed with finalizer removal.
- Avoids ERROR logs and warning events for idempotent delete (instance already gone).

### Fix 2: Aruba "cannot be deleted" retry ✅

**File:** `internal/controller/nodeclaim_controller.go`

- When Aruba returns "cannot be deleted" (state restriction): log INFO, record Normal event, return `RequeueAfter: 30s`.
- Controller retries delete until Aruba allows it; avoids blocking scale-down indefinitely.

### Fix 3: Azure VM size / ProvisioningFailed handling ✅

- **CloudBroker:** Added `excluded_instance_types` to the recommendations API. When a provisioning attempt fails, the autoscaler can request new recommendations excluding the failed instance type.
- **NodeClaim controller:** When `CreateInstance` fails and there are no alternatives left:
  1. If `Spec.RecommendationRequest` is set and CloudBroker client is available, call CloudBroker with `excluded_instance_types=[failed_instance_type]`.
  2. If new recommendations are returned, update `Chosen` and `Alternatives`, patch the NodeClaim, and requeue.
  3. Otherwise, mark as Failed as before.
- **NodePool:** Now sets `Spec.RecommendationRequest` when creating NodeClaims so the NodeClaim controller can retry with fresh recommendations.

---

## Additional Fixes (2026-03-04, iteration 2)

### Fix 4: Azure Premium_LRS for A-series VMs ✅

**File:** `internal/azure/provisioner.go`

- Changed OS disk `StorageAccountType` from `Premium_LRS` to `Standard_LRS`.
- A-series (e.g. Standard_A2_v2) and other Basic-tier sizes do not support Premium storage.
- Standard_LRS works with all Azure VM sizes.

### Fix 5: CloudBroker Azure ARM filter for x86_64 ✅

**File:** `cloudbroker/app/services/recommendation_service.py`

- Azure ARM-only sizes (pls, ps_v5, plds) may be mislabeled as x86_64 in the DB.
- When requesting x86_64, exclude Azure instance types matching `%pls%`, `%ps_v5%`, `%plds%`.
- Prevents "Arm64 VM size with x64 image" provisioning failures.

---

## Verification

After fixes:

1. Re-run `./hack/integration-test-e2e-all-providers.sh`.
2. Inspect `controller-e2e-all-providers.log` for:
   - No ERROR for 404 on delete (or explicit "instance already deleted" INFO log).
   - Aruba: "not ready for delete, will retry" then eventual success.
   - Azure: no Premium_LRS or ARM/x64 mismatch errors.
3. Confirm graph dump (`graph-e2e-all-providers.json`) and UI show no overlapping nodes.
