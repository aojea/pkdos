# krun

Execute commands in parallel in Pods


## Installation

```sh
make build
# Binary will be available at ./bin/krun
```

## Usage

### Parallel Command Execution

Execute a shell command on all pods matching a label selector:

```sh
$ kubectl apply -f examples/statefulset.yaml
statefulset.apps/krun-test-web created
```

```sh
$ kubectl wait --for=condition=Ready pod/krun-test-web-0 pod/krun-test-web-1 --timeout=60s
pod/krun-test-web-0 condition met
pod/krun-test-web-1 condition met
```

```sh
$ make build
Building all binaries...
GOOS= GOARCH= go build -o ./bin/krun ./krun
```

```sh
./bin/krun   --namespace="default"   --label-selector="app=krun-test"   --command="hostname"
[krun-test-web-1] krun-test-web-1
[krun-test-web-0] krun-test-web-0
```

### File Synchronization (Upload)

Upload a local file or directory to all matching pods. This uses a streaming `tar` approach, so `tar`
command is expected to exist on the destination Pods.

```sh
# Upload local './config' folder to '/tmp/config' on all pods
./bin/krun \
  --label-selector "app=worker" \
  --upload-src "./config" \
  --upload-dest "/tmp/config"
```

### Upload and Execute (Script piping)

You can combine upload and execution to run local scripts on remote pods.
The tool guarantees the upload finishes before the command starts.

```sh
# 1. Upload the script
# 2. Execute it immediately
./bin/krun \
  --label-selector "app=database" \
  --upload-src "./scripts/maintenance.sh" \
  --upload-dest "/tmp/maintenance.sh" \
  --command "/bin/sh /tmp/maintenance.sh"
```

### Upload Custom Binary and Run

Distribute a custom tool or binary and run it.

```sh
/bin/krun \
  --label-selector "app=backend" \
  --upload-src "./bin/my-debug-tool" \
  --upload-dest "/usr/local/bin/my-debug-tool" \
  --command "/usr/local/bin/my-debug-tool --verbose"
```
