# Per-Node Sub-CRDs with Falco Instance Operator as Coordinator

## Context

Artifact controllers (Rulesfile, Plugin, Config) run as a DaemonSet — one instance per node. All instances write to the same `status.conditions[]` on the same parent CRD, causing a last-write-wins race where a healthy node can overwrite a failing node's `Programmed=False`.

The fix: give each node its own object (single writer, no race), with a singleton coordinator that owns the aggregate write to the parent. The coordinator already exists: the **Falco instance operator** runs as a singleton Deployment with RBAC for `nodes`, `rulesfiles`, `plugins`, and `configs` already declared.

---

## Division of responsibility

| Role | Owner |
|------|-------|
| Create `RulesfileNode`/`PluginNode`/`ConfigNode` objects per matching node | **Falco instance operator** |
| Delete node objects when a node no longer matches the selector | **Falco instance operator** |
| Write aggregate conditions to the parent artifact (`Rulesfile.status.conditions`) | **Falco instance operator** |
| Reconcile local filesystem; write per-node conditions to own node object only | **Artifact operator** (per-node DaemonSet) |

The artifact operator **never writes to the parent artifact's status**.

---

## New CRDs: `RulesfileNode`, `PluginNode`, `ConfigNode`

Three internal CRDs, one per artifact type. The spec is minimal — the node object is an assignment signal and per-node status bucket; the artifact operator always reads the parent artifact for the actual work spec (OCI image, inline rules, configmap ref, etc.).

### `RulesfileNode` (`api/artifact/v1alpha1/rulesfilenode_types.go`)

```go
type RulesfileNodeSpec struct {
    // NodeName is the node to which this rulesfile is assigned.
    NodeName string `json:"nodeName"`
}

type RulesfileNodeStatus struct {
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=rulesfilenodes,categories=artifacts
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Programmed",type="string",JSONPath=".status.conditions[?(@.type=='Programmed')].status"
type RulesfileNode struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   RulesfileNodeSpec   `json:"spec,omitempty"`
    Status RulesfileNodeStatus `json:"status,omitempty"`
}
```

Same pattern for `PluginNode` and `ConfigNode`.

**Naming**: `<artifact-name>--<node-name>` (double-dash separator).  
If combined length > 253 chars: `<artifact-name-truncated>--<sha256[:8](full-name)>`.

**Labels** (for listing siblings):
```
artifact.falcosecurity.dev/parent: <artifact-name>
artifact.falcosecurity.dev/node:   <node-name>
```

**Owner reference**: → parent artifact. GC cascade deletes all node objects when parent is deleted.

---

## Falco instance operator: new artifact aggregator controllers

Add three new reconcilers inside the instance operator manager (alongside the existing `configmap` and `secret` reference controllers):

```
controllers/instance/artifact/rulesfile/controller.go
controllers/instance/artifact/plugin/controller.go
controllers/instance/artifact/config/controller.go
```

### Reconcile loop (same pattern for all three)

```
Reconcile(Rulesfile):
  1. List all Nodes matching Rulesfile.Spec.Selector
  2. For each matching node → ensure RulesfileNode/<name>--<node> exists (create + set ownerRef + labels)
  3. List all existing RulesfileNode for this Rulesfile (label: parent=<name>)
  4. For any RulesfileNode whose node is not in the matching set → delete it
  5. Compute aggregate conditions from all RulesfileNode.Status.Conditions:
       Programmed=True   iff ALL node objects have Programmed=True
       Programmed=False  if  ANY node object has Programmed=False
                         message: "N of M nodes not programmed: node-a, node-b, ..."
       Programmed=Unknown if none False and any Unknown
  6. Write aggregate to Rulesfile.Status.Conditions
     field manager: "instance-artifact-rulesfile"
```

### Watches

```go
ctrl.NewControllerManagedBy(mgr).
    For(&artifactv1alpha1.Rulesfile{}).
    Watches(&corev1.Node{},
        handler.EnqueueRequestsFromMapFunc(findRulesfilesForNode),
    ).
    Watches(&artifactv1alpha1.RulesfileNode{},
        handler.EnqueueRequestsFromMapFunc(func(ctx, obj) []Request {
            parent := obj.GetLabels()["artifact.falcosecurity.dev/parent"]
            return []Request{{NamespacedName: {Name: parent, Namespace: obj.Namespace}}}
        }),
    ).
    Complete(r)
```

When any `RulesfileNode` status changes, the singleton recomputes and writes the aggregate.

---

## Artifact operator: changes

### Reconcile on `RulesfileNode` — dual-trigger watch

The reconcile queue item is always a `RulesfileNode` NamespacedName, but changes to the **parent `Rulesfile`** also trigger reconciliation via a map function. This ensures that when a user updates the Rulesfile spec (new OCI tag, inline rules, changed selector, new label, etc.), all per-node operators are immediately re-triggered even though the `RulesfileNode` objects themselves have not changed.

```go
ctrl.NewControllerManagedBy(mgr).
    // Trigger 1: my RulesfileNode was created, updated, or its status changed.
    For(&artifactv1alpha1.RulesfileNode{},
        builder.WithPredicates(matchesNode(r.nodeName)),  // only my node's objects
    ).
    // Trigger 2: parent Rulesfile spec (or metadata) changed — map to my RulesfileNode.
    // Without this, an OCI tag bump or label change on the Rulesfile would not reach
    // the per-node operators until the RulesfileNode itself is touched.
    Watches(&artifactv1alpha1.Rulesfile{},
        handler.EnqueueRequestsFromMapFunc(func(ctx, obj) []Request {
            name := nodeObjectName(obj.GetName(), r.nodeName)
            return []Request{{NamespacedName: {Name: name, Namespace: obj.Namespace}}}
        }),
    ).
    Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.findNodeObjectsForSecret)).
    WatchesRawSource(source.Channel(versionEvents, ...)).
    Complete(r)
```

In all cases the reconcile body reads the parent `Rulesfile` spec to get the actual work configuration — the `RulesfileNode` carries only node assignment and per-node status.

### Reconcile loop

```
Reconcile(RulesfileNode):
  1. Get the RulesfileNode — if not found, return (GC'd or not yet created)
  2. Get parent Rulesfile via ownerRef → this is the source of truth for spec
  3. Handle deletion: if DeletionTimestamp set → cleanup local FS → remove finalizer → return
  4. Ensure finalizer on RulesfileNode (replaces the old per-node finalizer on the parent)
  5. Run existing logic: reference resolution, OCI pull, compat checks, write to FS
  6. Write resulting conditions to RulesfileNode.Status.Conditions only
     field manager: "artifact-rulesfile/<nodeName>"
```

The `NodeMatchesSelector` check is removed — existence of the `RulesfileNode` is the assignment signal.

### Finalizer design

**Parent artifact** (`Rulesfile`): a single in-use finalizer managed by the **instance operator aggregator**.
- Added when the first `RulesfileNode` is created for this artifact.
- Removed when all `RulesfileNode` objects for this artifact have been deleted.
- Blocks parent deletion until all per-node cleanup is complete.

**`RulesfileNode`**: one finalizer per node object, managed by the **per-node artifact operator**.
- Added when the artifact operator first reconciles the node object.
- Removed only after local filesystem resources (rules files, plugin .so, config fragments) have been cleaned up.

This replaces the old scheme where each artifact operator placed a named finalizer (`rulesfile.artifact.falcosecurity.dev/finalizer/<nodeName>`) directly on the parent. The artifact operator **no longer touches the parent artifact's finalizers at all**.

**Deletion flow:**
```
User deletes Rulesfile
  → Rulesfile enters Terminating (instance operator's finalizer holds it)
  → GC cascade: DeletionTimestamp set on all owned RulesfileNode objects
      → each per-node artifact operator detects DeletionTimestamp on its RulesfileNode
         → cleans up local FS resources
         → removes finalizer from its RulesfileNode
         → RulesfileNode is deleted
  → instance operator watches RulesfileNode deletions
     → when all are gone → removes finalizer from parent Rulesfile
  → Rulesfile is fully deleted
```

---

## Data flow summary

```
[Falco instance operator]
  Rulesfile/my-rules (spec: ociArtifact, selector)
    → creates RulesfileNode/my-rules--node-1  (nodeName: node-1)
    → creates RulesfileNode/my-rules--node-2  (nodeName: node-2)
    → creates RulesfileNode/my-rules--node-3  (nodeName: node-3)
    ← reads all RulesfileNode.status.conditions
    → writes Rulesfile.status.conditions (aggregate, sole writer)

[Artifact operator, node-1]
  watches RulesfileNode/my-rules--node-1
    → reads parent Rulesfile spec (OCI image, etc.)
    → pulls artifact, writes to /etc/falco/rulesfiles/...
    → writes RulesfileNode/my-rules--node-1.status.conditions
```

---

## Files to create or modify

| File | Change |
|------|--------|
| `api/artifact/v1alpha1/rulesfilenode_types.go` | New |
| `api/artifact/v1alpha1/pluginnode_types.go` | New |
| `api/artifact/v1alpha1/confignode_types.go` | New |
| `api/artifact/v1alpha1/groupversion_info.go` | Register new types |
| `api/artifact/v1alpha1/zz_generated.deepcopy.go` | Regenerated |
| `chart/falco-operator/crds/` | Three new CRD YAMLs (`make manifests`) |
| `controllers/instance/artifact/rulesfile/controller.go` | New — aggregator |
| `controllers/instance/artifact/plugin/controller.go` | New — aggregator |
| `controllers/instance/artifact/config/controller.go` | New — aggregator |
| `cmd/instance/main.go` | Register three new aggregator controllers |
| `controllers/artifact/rulesfile/controller.go` | Reconcile on RulesfileNode; read parent for spec; write to node only; finalizer on node |
| `controllers/artifact/plugin/controller.go` | Same |
| `controllers/artifact/config/controller.go` | Same |
| Tests for all changed controllers | Update/add |

---

## Progress

Tasks completed so far:

- [x] New CRD types: `RulesfileNode`, `PluginNode`, `ConfigNode` (with deepcopy, groupversion registration, CRD YAMLs)
- [x] Artifact controller — `rulesfile/controller.go`: reconciles `RulesfileNode`, reads parent via ownerRef, writes conditions to node only
- [x] Artifact controller — `plugin/controller.go`: same pattern
- [x] Artifact controller — `config/controller.go`: same pattern
- [x] Tests for all three artifact controllers (unit + integration): 209 tests passing
- [ ] Instance operator aggregator controllers (`controllers/instance/artifact/*/controller.go`)
- [ ] Register aggregator controllers in `cmd/instance/main.go`
- [ ] End-to-end verification on a multi-node kind cluster

---

## Verification

1. `make generate && make manifests` — three new CRD YAMLs, no schema errors
2. `KUBEBUILDER_ASSETS=... go test ./controllers/artifact/...` — 209 tests pass
3. Multi-node test (kind 3-node cluster):
   - Create a `Rulesfile` → instance operator creates 3 `RulesfileNode` objects
   - Each artifact operator reconciles its own node object, writes per-node conditions
   - `Rulesfile.status.conditions[Programmed]=True` once all 3 are True
   - Force failure on one node → its `RulesfileNode` becomes `Programmed=False`
   - Verify parent stays `Programmed=False`; healthy nodes do NOT flip it
   - Recover → all become `True`, parent follows
   - Delete the `Rulesfile` → all `RulesfileNode` objects garbage-collected
