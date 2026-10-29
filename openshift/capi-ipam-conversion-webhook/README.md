# capi-ipam-conversion-webhook

This binary serves `/convert` using the existing Cluster API conversion code.
It supports IPAM (`v1alpha1`, `v1beta1`, `v1beta2`) resources only. Core, addons,
and runtime scheme registrations are commented out, so those conversions are rejected.
It does not start CAPI controllers, admission webhooks, or leader election.
Cluster-profile mode starts only the shared TLS profile watcher and its cache.

## Planned deployment and retirement

This is a deployment and upgrade plan, not an implemented manifest bundle.

Release N:
- introduces the standalone conversion webhook for Machine API Provider vSphere (MAPV)'s core CAPI IPAM CRDs.

Release N+1:
- promotes `ClusterAPIMachineManagement` into the Default feature set,
- transfers IPAM CRDs management to the Cluster CAPI Operator (CCAPIO)
- intrinsecally transfers the IPAM CRDs conversion responsibility to the real core CAPI controller manager,
- and removes the standalone conversion webhook in the same upgrade.

The webhook uses `openshift-cluster-api`, which becomes shared with the full
core CAPI controller manager. Neither the namespace nor the IPAM CRDs must be
deleted during retirement.

### Release N: introduce the standalone webhook

1. Add a multi-document payload bundle at:

   `openshift/manifests/0000_30_cluster-api_03_capi-ipam-conversion-webhook.yaml`

   Use dedicated `capi-ipam-conversion-webhook` names and selectors for:

   - ServiceAccount.
   - ClusterRole granting only `get`, `list`, and `watch` on
     `apiservers.config.openshift.io`.
   - ClusterRoleBinding.
   - Service exposing port 443 to the named `webhook-server` port at 9443.
   - Deployment with one replica (no need to leader election) and no PodDisruptionBudget.
   - NetworkPolicy selecting only the standalone webhook pods, allowing
     conversion traffic on TCP 9443 and required outbound API/DNS connectivity.

   Ensure the `openshift-cluster-api` namespace is installed when MachineAPI or ClusterAPI is enabled.
   Its existing CAPI payload gating
   must not leave the standalone resources without a namespace. Coordinate the
   namespace definitions and security metadata with CCAPIO; it is a shared
   resource, not part of the later deletion inventory.

2. Apply these annotations to the standalone runtime resources:

   ```yaml
   # capability.openshift.io/name: MachineAPI # TODO(damdo): to verify if we want this or not on the namespace, ok for the other resources.
   exclude.release.openshift.io/internal-openshift-hosted: "true"
   include.release.openshift.io/self-managed-high-availability: "true"
   include.release.openshift.io/single-node-developer: "true"
   ```

   Do not negatively gate the runtime resources on
   `ClusterAPIMachineManagement`. The promotion will be independent from the deletion,
   which will be handled by tombstoning.

3. Request the dedicated serving certificate through the Service:

   ```yaml
   service.beta.openshift.io/serving-cert-secret-name: capi-ipam-conversion-webhook-cert
   ```

   Do not ship a TLS Secret. Mount the service-ca-generated Secret read-only
   and non-optionally at `/tmp/k8s-webhook-server/serving-certs`, using the
   whole directory rather than `subPath` so certificate reload works.

4. Configure the Deployment to:

   - Run `/capi-ipam-conversion-webhook` with normal API-driven TLS settings,
     without explicit TLS overrides.
   - Use HTTPS startup/liveness probes on `/healthz` and readiness on `/readyz`
     at port 9443.
   - Use control-plane placement, master/control-plane taint tolerations,
     management workload scheduling, and `system-cluster-critical` priority.
   - Use restricted-v2-compatible security: non-root execution, no privilege
     escalation, dropped capabilities, RuntimeDefault seccomp, and no fixed
     UID/GID. Verify read-only-root-filesystem compatibility.
   - Use rolling updates with `maxUnavailable: 0` and `maxSurge: 1`.
   - Start with requests of 10m CPU and 50Mi memory, then validate usage.

   Keep its labels distinct from `cluster.x-k8s.io/provider: cluster-api` so
   the full manager's Service cannot accidentally select the IPAM-only server.
   The kubelet restarts the process after a TLS profile change shuts it down.

5. Update standalone IPAM generation in `openshift/ipam/kustomization.yaml`.
   Keep both CRDs in their generated payload file, with MachineAPI capability
   gating and the negative feature gate:

   ```yaml
   capability.openshift.io/name: MachineAPI
   release.openshift.io/feature-gate: "-ClusterAPIMachineManagement"
   service.beta.openshift.io/inject-cabundle: "true"
   ```

   Point their conversion configuration at:

   ```yaml
   name: capi-ipam-conversion-webhook
   namespace: openshift-cluster-api
   path: /convert
   port: 443
   ```

   Preserve all schemas, serve `v1alpha1`, `v1beta1`, and `v1beta2`, and retain
   `v1beta1` as the only storage version. Leave full-provider CRD endpoint
   references unchanged in this release.

6. Build the isolated module in the component image, copy the binary to
   `/capi-ipam-conversion-webhook`, and wire release image substitution to the
   existing `cluster-capi-controllers` image. Keep `/manager` as the image's
   default entrypoint; the standalone Deployment overrides it.

7. Validate fresh Default installations, HA and single-node scheduling,
   capability inclusion, namespace network policies, CA injection, all IPAM
   conversions, certificate rotation, and TLS-triggered restarts.

### Release N+1: promote, adopt, and clean up in the same release

1. At payload order 30, begin the ownership and conversion handoff by promoting
   `ClusterAPIMachineManagement` into the Default feature set.

   The negatively gated standalone CRD manifests
   (the IPAM CRDs managed by CVO under openshift/manifests) become excluded,
   leaving their live unmanaged CRDs and stored objects intact.
   The Positively gated CCAPIO operator now becomes active in Default.

2. Have CCAPIO adopt both existing IPAM CRDs through Boxcutter. Keep this
   annotation in the provider's desired CRD manifests:

   ```yaml
   capi-operator.openshift.io/adopt-existing: "always"
   ```

   It already exists in `openshift/patches/ipam-crd-adopt-existing.yaml`.
   This is an annotation, not a label; setting it only on the live object is
   insufficient. Boxcutter consumes it to disable collision protection and
   strips it before applying the object.

   Do not delete and recreate the CRDs.

3. Have CCAPIO, alongside the CRDs, install the core CAPI manager and its webhooks,
   while applying the fresh CRDs switch both IPAM CRDs to use the new conversion endpoints:

   ```yaml
   name: capi-webhook-service
   namespace: openshift-cluster-api
   path: /convert
   port: 443
   ```

   Preserve CA injection and validate the effective CA bundle and cross-version
   requests through the Kubernetes API server. The current installer applies
   CRDs before other provider resources: that can switch references before a
   newly created manager is ready. The CCAPIO installer behaviour + CVO completion barrier
   below protects later cleanup, but does not eliminate this earlier transition window.
   Validate its acceptability or adjust the installation sequence for a readiness-aware
   endpoint switch.

4. In this release, remove the standalone IPAM CRD definitions from openshift/cluster-api's
   `openshift/manifests/`, including their generation there, or retain their
   negative feature gate so CVO no longer reconciles them in promoted clusters.
   Removal is appropriate only when all supported configurations have another
   owner. Never add CRD deletion tombstones.

5. Use the existing order-30 completion barrier: CCAPIO already reports the
   target operator version only after the desired CAPI provider revision finishes
   installation; no new version-reporting mechanism is required for this plan.
   Boxcutter applies the provider revision and probes CRDs for
   `Established=True` and provider Deployments, including the core CAPI manager,
   for `Available=True`.

6. The core manager Deployment probes `/readyz`. CAPI registers
   `mgr.GetWebhookServer().StartedChecker()` there, which checks that the
   webhook listener has started and accepts a local TLS connection.

7. The installer advances `ClusterAPI.status.currentRevision` after all phases
   complete. The revision controller reports the target operator version only
   when the current revision matches the desired revision.

8. During an ordered upgrade, CVO checks the order-30 `ClusterOperator/cluster-api`
   for availability, absence of degradation, and the expected target version
   before advancing to order 31. `Progressing=True` alone is not a barrier.

   This is meaningful replacement-webhook readiness, not merely readiness of the
   CCAPIO operator Deployment. It supports same-release cleanup after installation
   of the replacement provider revision.

   Implementation references reviewed for this plan:

   - [CCAPIO revision/version reporting](https://github.com/openshift/cluster-capi-operator/blob/main/pkg/controllers/revision/revision_controller.go)
   - [Boxcutter phases and adoption](https://github.com/openshift/cluster-capi-operator/blob/main/pkg/controllers/installer/boxcutter.go)
   - [Installer readiness probes](https://github.com/openshift/cluster-capi-operator/blob/main/pkg/controllers/installer/probes.go)
   - [CAPI readiness registration](../../main.go)
   - [CVO ordered upgrade scheduling](https://github.com/openshift/cluster-version-operator/blob/main/pkg/cvo/sync_worker.go)
   - [CVO ClusterOperator health/version checks](https://github.com/openshift/cluster-version-operator/blob/main/pkg/cvo/internal/operatorstatus.go)

9. Prepare deletion of the standalone capi-ipam-conversion-webhook resources at payload order 31 by removing
   the standalone runtime creation manifests from the target payload.
   Keep the existing source-release webhook serving until handoff; target-payload
   omission does not delete it. Do not rely on its image being updated during
   this transition.

10. Add an order-31 cleanup bundle, for example:

    in `openshift/manifests/0000_31_cluster-api_00_delete-capi-ipam-conversion-webhook.yaml`

    Add `release.openshift.io/delete: "true"` to tombstones for the dedicated:

    - Deployment.
    - Service.
    - ServiceAccount.
    - ClusterRoleBinding.
    - ClusterRole.
    - NetworkPolicy.

    Retain applicable release-profile annotations and coordinate capability
    eligibility with the order-30 barrier. Do not retain a negative feature gate
    that excludes deletion after promotion. Tombstones must themselves pass
    CVO's inclusion filters; do not ship active creation and deletion manifests
    for the same temporary resource in the target payload.

11. Verify the generated Secret's owner reference points to the dedicated
    Service and that deletion garbage-collects it. Explicitly remove an orphan
    only after checking exclusive use and successful handoff. Do not delete the
    full manager's serving certificate.

12. Never tombstone either IPAM CRD, `openshift-cluster-api`, or any other shared
    manager resources. CRD tombstones issue actual deletion, not relinquishment
    of CVO ownership. The namespace must remain for the replacement manager;
    the earlier dedicated-namespace deletion plan no longer applies.

13. Verify actual removal: CVO deletion is asynchronous. Retain deletion coverage
    for supported source-to-target upgrade paths, including relevant z-stream
    entry points and any supported skips. Remove obsolete binary packaging/code
    only when no target-payload workload still needs it.

14. Validate the upgrade acceptance criteria:

    - Existing beta1-stored objects survive adoption and remain usable through all
      served IPAM versions.
    - After handoff, both CRDs reference the core manager Service with valid CA trust.
    - Delaying/failing the replacement manager keeps CCAPIO from reporting the target
      version and leaves the standalone resources intact before order 31.
    - Successful installation satisfies the existing order-30 barrier, then order-31
      deletion removes only the standalone resources.
    - Capability-disabled configurations cannot bypass a required handoff barrier or
      lose their only IPAM conversion endpoint.
    - Fresh promotion-release installs do not create the tombstoned webhook; upgrade
      and fresh-install namespace ownership remain valid.
    - The shared namespace, full manager, IPAM CRDs, and stored objects survive cleanup.

    There is no planned Release N+2 cleanup phase: retirement occurs in Release N+1
    after the existing order-30 barrier, subject to the validation above.
