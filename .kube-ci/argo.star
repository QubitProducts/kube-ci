def addVolumeMountToTemplate(t, name, mount_path):
    cont = dict(t.get("container", {}))
    if not cont:
        return t

    mounts = list(cont.get("volumeMounts", []))
    mounts.append({
              "name": name,
              "mountPath": mount_path,
            })
    t["container"]["volumeMounts"] = mounts
    return t


def addContainerInputsToTemplate(t, extra_inputs):
    cont = dict(t.get("container", {}))
    if not cont:
        return t

    inputs = list(cont.get("inputs", []))
    inputs = inputs + extra_inputs

    t["container"]["inputs"] = inputs
    return t


# add_volume_to_spec adds a volume to the workflow spec, and a
# volume mount to all templates that include a container definition
def add_volume_to_spec(spec={}, vol={}, mount_path=""):
    if not vol:
        return spec

    spec = dict(spec)
    vols = list(spec.get("volumes", []))
    tmpls = list(spec.get("templates", []))

    vols.append(vol)

    tmpls = [addVolumeMountToTemplate(
                t,
                vol["name"],
                mount_path)
             for t in tmpls]

    spec["volumes"] = vols
    spec["tmpls"] = tmpls
    return spec


def container(
        name="",
        env=[],
        image="busybox:latest",
        working_dir="/src",
        command=["/bin/sh", "-c"],
        script="",
        ):
    cont = {
            "name": name,
            "args": [script],
            "command": command,
            "env": env,
            "image": image,
            "workingDir": working_dir
          }
    return cont


def go_container(
        name="build",
        version="1.19",
        env=[],
        script="make test",
        ):

    envs = list(env)
    envs.append({
                    "name": "GOPROXY",
                    "value": "http://gomod-athens:8080"
                })
    return container(
              name=name,
              env=envs,
              image="golang:{}".format(version),
              script=script
           )


def containerInputGit(
        name="code",
        path="/src",
        git={
                "repo": "{{workflow.parameters.repo}}",
                "revision": "{{workflow.parameters.revision}}"
            },
        ssh_private_key_secret={
                                   "key": "ssh-private-key",
                                   "name": "ci-secrets"
                               }
        ):

    input = {"artifacts": [{"git": git, "name": name, "path": path}]}
    if ssh_private_key_secret:
        input["sshPrivateKeySecret"] = ssh_private_key_secret

    return input


def spec_from_steps(steps=[]):
    if not steps:
        return {}

    def _template_step(cont):
        return [{
          "name": cont["name"],
          "template": "run-" + cont["name"]
        }]

    def _template(cont):
        return {
          "name": "run-" + cont["name"],
          "container": cont
        }

    def _templates(steps):
        return [
           {
            "name": "start",
            "steps": [_template_step(s) for s in steps]
           }
        ] + [_template(s) for s in steps]

    step0 = steps[0]
    entrypoint = "start"

    return {
        "entrypoint": entrypoint,
        "templates": _templates(steps)
    }


def argo_workflow(
        cacheSize="",
        cacheScope="project",
        metadata={"annotations": {}},
        container_inputs=[containerInputGit()],
        spec={},
        ):

    wf = {
        "apiVersion": "argoproj.io/v1alpha1",
        "kind": "Workflow",
        "spec": spec
    }

    # add the git repo to all the templates with containers
    tmpls = list(spec.get("templates", []))
    if container_inputs:
        tmpls = [addContainerInputsToTemplate(
                          t,
                          container_inputs)
                 for t in tmpls]
    spec["templates"] = tmpls

    # add secrets to all the templates with containers
    spec = add_volume_to_spec(
                    spec=spec,
                    vol={
                            "name": "pod-ci-secrets",
                            "secret": {
                                "secretName": "ci-secrets"
                            }
                        },
                    mount_path="/ci-secrets")

    if cacheSize != "":
        # add cache to all the templates with containers
        metadata = dict(metadata)
        anns = dict(metadata.get("annotations", {}))

        anns["kube-ci.qutics.com/cacheScope"] = cacheScope
        anns["kube-ci.qutics.com/cacheSize"] = cacheSize

        claim_name = "{{workflow.parameters.cacheVolumeClaimName}}"
        spec = add_volume_to_spec(
                        spec=spec,
                        vol={
                              "name": "build-cache",
                              "persistentVolumeClaim": {
                                "claimName": claim_name
                              }
                            },
                        mount_path="/cache")

        metadata["annotations"] = anns
        wf["metadata"] = metadata

    wf["spec"] = spec

    return wf


def workflow_from_steps(
        cacheSize="",
        cacheScope="project",
        metadata={"annotations": {}},
        container_inputs=[containerInputGit()],
        steps=[]):

    spec = spec_from_steps(steps)
    return argo_workflow(
            cacheScope=cacheScope,
            cacheSize=cacheSize,
            metadata=metadata,
            container_inputs=container_inputs,
            spec=spec,
    )
