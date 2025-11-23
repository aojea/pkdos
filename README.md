# krun

krun is a command-line tool designed to simplify AI/ML workflows on Kubernetes by providing the ability to execute commands in parallel in Pods and handle file synchronization (upload) to multiple targets concurrently.

## Installation

You can build the binary using the provided Makefile:

```sh
make build
# Binary will be available at ./bin/krun
```

## Usage

The `krun` tool has two primary subcommands: `run` for general Pod-based operations using a label selector, and `jobset` for operations targeting JobSet workloads.

### `krun run`: General Pod Execution

The `run` subcommand executes a command or uploads files to all pods matching a Kubernetes **label selector** (`--label-selector`).

| Flag | Description | Default |
| :--- | :--- | :--- |
| `-l, --label-selector` | Label selector for pods (e.g., `app=my-app`). **Required**. | |
| `--upload-src` | Local path to folder/file to upload. | |
| `--upload-dest` | Remote destination path (e.g., `/tmp/app`). **Required if** `--upload-src` is set. | |
| `--exclude` | Regex pattern to exclude files when uploading. | |
| `--timeout` | Timeout for the execution (e.g., `30s`). | 0 (no timeout) |

#### Parallel Command Execution

Execute a shell command on all matching pods. The command to run is passed after a double-dash (`--`).

```sh
# Example: Run 'hostname' on all pods labeled with app=backend
./bin/krun run --label-selector=app=backend -- hostname

# Output includes pod names as prefix
[krun-test-web-1] krun-test-web-1
[krun-test-web-0] krun-test-web-0
```

#### File Synchronization (Upload)

Upload a local file or directory to all matching pods concurrently. The upload mechanism uses a streaming `tar` approach, requiring the `tar` command to exist on the destination Pods.

```sh
# Upload local './examples' folder to '/tmp/examples' on all pods
./bin/krun run \
  --label-selector="app=krun-test" \
  --upload-src "./examples" \
  --upload-dest "/tmp/examples"
```

#### Upload and Execute (Script Piping)

Combine file upload and command execution to run local scripts remotely. The upload completes before the command starts.

```sh
# 1. Upload a local script/binary
# 2. Execute it immediately on the remote pods
./bin/krun run \
  --label-selector "app=database" \
  --upload-src "./scripts/maintenance.sh" \
  --upload-dest "/tmp/maintenance.sh" \
  -- /bin/sh /tmp/maintenance.sh
```

### `krun jobset`: JobSet Workflows

This command group provides specific tools for managing Kubernetes JobSet workloads, often used for high-performance or distributed training applications.

#### `krun jobset run` (Execute/Upload on JobSet Pods)

Works like `krun run`, but targets pods belonging to a specific JobSet identified by `--name`.

| Flag | Description | Default |
| :--- | :--- | :--- |
| `-j, --name` | **Name of the JobSet** to target. **Required**. | |
| `--exclude` | Regex pattern to exclude files/folders. | `(^|/)\.` (excludes all hidden files and folders) |

```sh
# Run a command on pods belonging to a JobSet named 'stoelinga'
krun jobset run --name=stoelinga -- pip install -r requirements.txt

# Upload files and run a script on all JobSet pods
krun jobset run \
  --name=stoelinga \
  --upload-src=./bin \
  --upload-dest=/tmp/bin \
  -- /tmp/bin/start.sh
```

#### `krun jobset launch` (Launch a JobSet)

This subcommand generates and creates a JobSet manifest, useful for launching hardware-accelerated workloads like TPUs.

| Flag | Description | Default |
| :--- | :--- | :--- |
| `--tpu-type` | Type and topology of TPU to launch (e.g., `v5p-32`, `tpu7x-16`). | `tpu7x-16` |
| `--image` | Container image to use for the TPU workers. | `gcr.io/tensorflow/tensorflow:latest` |

```sh
# Launch a JobSet named 'tpu-job' with a v5p-32 topology
krun jobset launch \
  --name=tpu-job \
  --tpu-type=v5p-32 \
  --image=my-custom-ml-image:latest
```

## Development and Testing

The project uses Go for the main binary and bats for integration tests.

### Running Integration Tests
Integration tests require `bats` and `kind` (Kubernetes in Docker) installed.


Run the full suite of tests:

```sh
bats tests/
```

To troubleshoot individual tests, you can use:

```sh
bats -x -o _artifacts --print-output-on-failure --filter "mytest" tests
```