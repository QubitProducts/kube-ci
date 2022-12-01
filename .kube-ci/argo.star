def addCacheToTemplate(t):
    cont = dict(t.get("container",{}))
    if not cont:
        return t

    mounts = list(cont.get("volumeMounts",[]))
    mounts.append({
              "mountPath": "/cache",
              "name": "build-cache"
            })
    t["container"]["volumeMounts"] = mounts
    return t

def argoWorkflow(
        cacheSize="",
        cacheScope="project",
        metadata={"annotations": {}},
        spec={}):

    wf = {
        "apiVersion": "argoproj.io/v1alpha1",
        "kind": "Workflow",
        "spec": spec
    }

    anns = dict(metadata.get("annotations", {}))
    vols = list(spec.get("volumes", []))
    tmpls = list(spec.get("templates", []))

    # add the cache
    if cacheSize != "":
        anns["kube-ci.qutics.com/cacheScope"] = cacheScope
        anns["kube-ci.qutics.com/cacheSize"] = cacheSize

        vols.append({
              "name": "build-cache",
              "persistentVolumeClaim": {
                "claimName": "{{workflow.parameters.cacheVolumeClaimName}}"
              }
            })
        tmpls = [addCacheToTemplate(t) for t in tmpls]

    metadata = dict(metadata)
    metadata["annotations"] = anns

    wf["metadata"] = metadata
    wf["spec"]["volumes"] = vols
    wf["spec"]["templates"] = tmpls

    return wf
