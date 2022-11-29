# Starlark Configuration for Kube CI

*NOTE*: This is preliminary work and much of this is likely to
be quite volatile for some time.

Starlark is a language specialised for configuration, based on
a reduced Python dialect.

kube-ci can load the workflow definitions by executing a [Starlark](https://github.com/google/starlark-go)
script housed in the repository being processed. Upon completion the
script should leave a workflow definition, represented as a `dict`
in to the value of a variable called `workflow`.

```python
entrypoint = "test"
workflow = {
		"apiVersion": "argoproj.io/v1alpha1",
		"kind": "Workflow",
		"spec": {
				"arguments": {},
				"entrypoint": entrypoint,
				"templates": [ { "name": "test"} ] } }
```

This has several advantages over directly supplying a workflow definition in YAML.

- Starlark allows provide reusable functions and libraries to make building workflows easier,
  with less syntax, and better, baked in, best practice.
- The `loadFile` and `load` (see below), allow us to pull in utility functions or data
  from other branches, or from completely different repositories.
- As well as allow for more abstraction, this also gives us the option of loading workflow
  definitions from the default branch of the repository, rather than the active branch. Or
  to store workflow definitions in a completely distinct repository.
- Some logic can be moved from the runtime of the workflow with argo, to the evaluation time
  of the starlark script, this allows the workflows within argo to be smaller and simpler,
  avoiding showing steps that are never relevant to a given run of a workflow.


Details of the CI job being run are passed to the script in a variable
called `input`. At present this contains the following information:

- `ref`: The git reference
- `ref_type`: The type of reference (`branch` or `tag`)
- `sha`: The commit being checked

TODO:
- `event`: The event that initiated the build
- repo: The repository being worked on
- sender: The entity that initiated the event

# Using Starlark utility libraries (load)

kube-ci lets you load starlark libraries. Several utility libraries
are provided directly by the server:

```python
load("builtin:///encoding/yaml", "yaml")
# you can now use, yaml.loads, and yaml.dumps to read and
# parse yaml data, see loadFile for the URL format
workflow = yaml.loads(loadFile("/ci.yaml"))

# There is a regexp library
load("builtin:///re", "compile")

# A JSON library
load("builtin:///encoding/json", "decode")

#... time handling, and maths

```

You can also load starlark files directly from the build
context.

```python
load("context:///definitions.star", "some_def")
```

Or from other locations in github, or the web

```python
load("github:///myrepo.myorg/utils/something.star", "some_def")
load("https:///example.com/utils/something.star", "some_def")
```

Within a module being loaded the scheme and host can be ignored, and
further modules will be loaded relative to the "current working path"

```python
load("///other.star", "some_def")
load("another.star", "some_other_def")
```

All modules have access to the current build context, as well as the
input variables.

# Loading external data (loadFile)

Raw data can be read using the `loadFile` builtin function.

It can be read from a github:

```
some_data = load("github://myrepo.myorg/somefile.txt")
```

Or directly from https URLs:
```
some_data = load("https://example.com/mydata.json")
```

# TODO

- Provide a full utility library to make building argo workflows easier, along the
  lines of existing python argo libraries.


