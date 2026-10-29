 # Upgrading CAPI IPAM CRDs into managing via CAPI operator
 
 This is a plan to transition the existing v1beta1 CAPI IPAM CRDs present in the Default feature set to be managed via CAPI operator and updated to v1beta2 in the process.
 
In short:

**Release N**: introduces CAPI IPAM CRDs v1beta2 as a served version, but not as a storage one. This is to fullfil the OpenShift API lifecycle requirement:
> We must not add a new version as storage, it must first go through a release where it is served but not storage.

It also introduces a standalone custom conversion webhook for Machine API Provider vSphere (MAPV)'s core CAPI IPAM CRDs, to allow v1beta1 <-> v1beta2 conversion.

**Release N+1**: promotes ClusterAPIMachineManagement into the Default feature set, transfers IPAM CRDs management to the Cluster CAPI Operator (CCAPIO), intrinsecally transfers the IPAM CRDs conversion responsibility to the real core CAPI controller manager, and removes the standalone conversion webhook during the same upgrade.


## In detail

 ## Release N: introduce the standalone webhook, tweak manifests

 ### 1. Introduce the deployment bundle for `capi-ipam-conversion-webhook`

 Create a new manifest in [openshift/cluster-api](https://github.com/openshift/cluster-api/tree/main/openshift) under `openshift/manifests/0000_30_cluster-api_03_capi-ipam-conversion-webhook.yaml` at payload order 30.
 This is the same order as the other cluster-capi-operator manifests.

 Include:
 - ServiceAccount.
 - ClusterRole: only get/list/watch on apiservers.config.openshift.io (to evaluate this as one might already exist?).
 - ClusterRoleBinding (same as above?).
 - Service and serving-certificate annotation
     - Request the dedicated serving certificate through the Service annotation `   service.beta.openshift.io/serving-cert-secret-name: capi-ipam-conversion-webhook-cert`, Do not ship a TLS Secret. Mount the service-ca-generated Secret read-only and non-optionally at `/tmp/k8s-webhook-server/serving-certs`, using the whole directory rather than subPath so certificate reload works.
 - Deployment: one replica, no PDB.
 - Dedicated NetworkPolicy.
 - All namespaced with `openshift-cluster-api` (when namespaceable)

 Use `capi-ipam-conversion-webhook` for the dedicated resource names and selectors.
 
 Ensure the `openshift-cluster-api` namespace is installed when MachineAPI or ClusterAPI is enabled (at the moment it's gated on `ClusterAPIMachineManagement`).
 
 The existing CAPI payload gating must not leave the standalone resources without a namespace. Coordinate the namespace definitions and security metadata with CCAPIO; it is a shared resource, not part of the later deletion inventory.

 ### 2. Gate the runtime resources

 Apply to and every webhook resource:

 ```yaml
   capability.openshift.io/name: MachineAPI
   exclude.release.openshift.io/internal-openshift-hosted: "true"
   include.release.openshift.io/self-managed-high-availability: "true"
   include.release.openshift.io/single-node-developer: "true"
 ```
 
  Do not negatively gate the runtime resources on `ClusterAPIMachineManagement`. The promotion will be independent from the deletion, which will be handled by tombstoning.


 ### 3. Configure the deployment

 - Run `/capi-ipam-conversion-webhook` with normal API-driven TLS settings, without explicit TLS overrides.
 - Service port 443 → pod port 9443.
 - Mount the generated certificate directory read-only, without subPath.
 - HTTPS startup/liveness probes on `/healthz`; readiness on `/readyz`.
 - API-driven TLS profile/adherence watching, with process restart on changes.
 - Control-plane placement, management scheduling, critical priority.
 - Use restricted-v2-compatible security: non-root execution, no privilege escalation, dropped capabilities, RuntimeDefault seccomp, and no fixed UID/GID. Verify read-only-root-filesystem compatibility.
 - Rolling updates: maxUnavailable: 0, maxSurge: 1.
 - NetworkPolicy covering webhook ingress and outbound API/DNS connectivity.
 - Keep its labels distinct from `cluster.x-k8s.io/provider: cluster-api` so the full manager's Service cannot accidentally select the IPAM-only server. The kubelet restarts the process after a TLS profile change shuts it down.
 - Package the binary from `openshift/capi-ipam-conversion-webhook/` in the existing component image and wire release image substitution.
 - Build the isolated module in the component image, copy the binary to `/capi-ipam-conversion-webhook`, and wire release image substitution to the existing cluster-capi-controllers image. Keep `/manager` as the image's default entrypoint; the standalone Deployment overrides it.
 - Validate fresh Default installations, HA and single-node scheduling, capability inclusion, namespace network policies, CA injection, all IPAM conversions, certificate rotation, and TLS-triggered restarts.

 ### 4. Configure standalone IPAM CRDs

 In openshift/cluster-api update standalone CAPI IPAM generation in `openshift/ipam/kustomization.yaml`:
 
 - Keep both CRDs in their generated payload file, with MachineAPI capability gating and the negative feature gate:
    - ```yaml
      capability.openshift.io/name: MachineAPI
      release.openshift.io/feature-gate: "-ClusterAPIMachineManagement"
      service.beta.openshift.io/inject-cabundle: "true"
      ```
 - Point both CRDs conversion configuration at the dedicated Service in the `openshift-cluster-api` namespace
     - ```yaml
       name: capi-ipam-conversion-webhook
       namespace: openshift-cluster-api
       path: /convert
       port: 443
        ```
 
 Preserve:
 - CA injection.
 - `v1alpha1/v1beta1/v1beta2` served versions (make sure we start deploying v1beta2 if it is not there!!!).
 - `v1beta1` only storage version!
 - Existing schemas and stored objects.
 - Leave full-provider CRD endpoint references unchanged in this release.


 ### 5. Prepare future adoption

 Keep this annotation on both CAPI IPAM CRDs in the full-provider generation inputs:

 ```yaml
   capi-operator.openshift.io/adopt-existing: "always"
 ```

 It already exists in openshift/patches/ipam-crd-adopt-existing.yaml. Boxcutter reads it from the desired manifest; it is not a label or readiness signal.

 ────────────────────────────────────────────────────────────────────────────────

 ## Release N->N+1 (upgrade): promote, adopt, and clean up in the same release

 ### TODO during N+1 dev-cycle
 #### Ensure adopt on CCAPIO managed CAPI IPAM CRDs manifests
 CCAPIO managed CAPI IPAM CRDs manifests must have the following annotation: 
   ```yaml
   capi-operator.openshift.io/adopt-existing: "always"
   ```
 #### Set CCAPIO-managed CAPI IPAM CRDs to v1beta2 storage
 Set `v1beta2` as the only storage version for both IPAM CRDs, retaining their
 `v1alpha1/v1beta1/v1beta2` served versions.
 Remove or update the downstream storage pin in
 `openshift/patches/ipam-crd-storage-version.yaml` and its generation references.

 Changing `storage: true` affects subsequent writes; it does not rewrite existing
 stored objects directly. The core CAPI manager's existing `CRDMigrator` automatically
 detects the storage-version change and migrates both IPAM kinds. It rewrites
 existing objects, updates `status.storedVersions` to `[v1beta2]`, and records
 the completed CRD generation. No separate migration request, Job, or controller
 is needed.

 #### Verify automatic IPAM storage migration
 Keep storage migration enabled in the target manager deployment. Validate in
 upgrade tests that existing objects remain usable, both CRDs report
 `status.storedVersions: [v1beta2]`, and their
 `crd-migration.cluster.x-k8s.io/observed-generation` annotation matches their
 current generation.
 
 #### Replace conversion webhook creation manifests with tombstones

 Remove the order-30 standalone webhook creation manifests from the N+1 payload.
 Add a new file `openshift/manifests/0000_31_cluster-api_03_capi-ipam-conversion-webhook-tombstone.yaml` to payload order 31, that deletes all the capi-ipam-conversion-webhook manifests (created at order 30):

 Use: `release.openshift.io/delete: "true"` do achieve this.

 Retain MachineAPI and applicable release-profile annotations. Do not add a negative feature gate that would exclude cleanup after promotion.

 The cleanup inventory comprises:
 - Deployment.
 - Service.
 - ServiceAccount.
 - NetworkPolicy.
 - ClusterRoleBinding.
 - ClusterRole.

 Never tombstone either CAPI IPAM CRD, the shared `openshift-cluster-api`
 namespace, `openshift-machine-api`, or any shared CAPI resource.
 `openshift-cluster-api` must remain because it hosts the replacement core CAPI manager.
 
 #### Promote ClusterAPIMachineManagement FG

 During release N+1 dev-cycle Promote ClusterAPIMachineManagement FG to the Default feature set.
 
 ### At Upgrade time

 - The standalone CVO managed CAPI IPAM CRDs definitions, now Negatively gated (`-ClusterAPIMachineManagement`), become excluded from CVO's apply set.
 
 - This stops CVO reconciling them (DO NOT TOMBSTONE THEM, WE NEED THEM)
 
 - Nevertheress, the existing standalone CAPI IPAM CRDs and CRs remain in the clusters (CVO doesn't delete missing manifests objects).
 
 - The positively gated (`ClusterAPIMachineManagement`) CAPI resources become enabled, subject to capability requirements (cluster-capi-operator turns on and starts applying core CAPI manifests).
 
 - The custom CAPI IPAM conversion webhook installed by Release N remains running until order-31 cleanup. Removing its creation manifests from the target payload does not delete the existing resources.
 
 - CCAPIO applies/adopts the core CAPI CRDs before applying the manager and other provider resources. Applying the IPAM CRDs switches their conversion references, potentially before the replacement webhook is ready. There could be a brief conversion hiccup until the standard conversion webhook is fully up and serving.

 - The new standard core CAPI manager conversion webhook (`/convert`) then starts to support both CAPI IPAM kinds and their served versions.

 - CCAPIO adopts existing CAPI IPAM CRDs through Boxcutter. keep this annotation in the provider's desired CRD manifests:

 ```yaml
   capi-operator.openshift.io/adopt-existing: "always"
 ```

 - CCAPIO comes up and uses boxcutter to apply the new set of managed manifests, it applies fresh CAPI IPAM CRDs, contain the following:
     - CA injection.
     - `v1alpha1/v1beta1/v1beta2` served versions (make sure v1beta2 is there!!!).
     - `v1beta2` as the only storage version (change from the previous v1beta1 being the storage one).
     - Existing schemas and stored objects.
     - Leave full-provider CRD endpoint references unchanged in this release.

 
 - This also automatically (by virtue of applying new CAPI IPAM CRD manifests) points both CRDs at the standard core CAPI conversion webhook endpoint/service:

     ```yaml
       name: capi-webhook-service
       namespace: openshift-cluster-api
       path: /convert
       port: 443
     ```
- This triggers the actual switch in target conversion webhook endpoints


 ──────────────────────────────────

 ## (SAME RELEASE, later upgrade stage) Release N->N+1: retire the custom conversion webhook, cleanup manifests

### At Upgrade time (continued)
 - CCAPIO has now deployed default core CAPI manager conversion works.Neither CAPI IPAM CRD references the standalone/custom conversion webhook. No supported capability configuration needs the fallback.
 
 - Before order-31 cleanup, the existing order-30 barrier establishes replacement webhook readiness:
     - Boxcutter waits for provider Deployments, including the core CAPI manager, to report `Available=True`.
     - The core manager's `/readyz` checks that its webhook listener has started and accepts a local TLS connection.
     - After all installation phases complete, CCAPIO advances `ClusterAPI.status.currentRevision` and reports the target operator version once it matches the desired revision.
     - CVO checks `ClusterOperator/cluster-api` for availability, absence of degradation, and the expected target version before advancing to order 31.

 - CVO applies the order-31 tombstones, deleting the standalone conversion webhook resources. The target payload no longer contains their order-30 creation manifests.
 
 ──────────────────────────────────

## Release N+2 cleanup
### TODO during N+2 dev-cycle
 #### Cleanup old CVO CAPI IPAM CRDs manifests
 - remove the standalone CAPI IPAM CRDs definitions from `openshift/manifests/` (CVO management path) (DO NOT TOMBSTONE THEM (the are in use and managed by CCAPIO))

 #### Cleanup old CVO CAPI IPAM conversion webhook tombstones
 - Remove the conversion webhook tombstones from `openshift/manifests/` only
   after verifying that every supported upgrade path has already performed
   cleanup. Retain them if any supported path can still encounter standalone
   webhook resources, including relevant z-stream entry points or supported skips.

