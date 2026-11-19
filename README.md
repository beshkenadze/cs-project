# cs-project

The launcher of a project VM for ContainerStack. In this mode, all services of one Compose project run in
one Apple VM, with one OCI container per service inside that VM.

`cs-project` is a static Linux arm64 program. It is the init process of the VM container. It reaps the
service processes and keeps their state. It also sends numbered events: a snapshot first, then each start
and exit. socktainer runs each other command as one exec in the VM, with JSON on stdin and stdout:

```
cs-project init | version | events
cs-project boot | create | start <id> | kill <id> <signal> | remove | hosts | cleanup <id>
```

For each service, the launcher does these steps:

- It unpacks the image layers once per project. Each layer goes on top of its parent chain.
- It mounts an overlay with a writable layer. A stop keeps the writable layer; a remove deletes it.
- It connects the service networks with the CNI `bridge` plugin.
- It runs the service with `runc`. The service gets the Docker default capability set, in its own cgroup.

## Build and test

You need Go 1.27.1 or later. `build.sh` runs on macOS and needs `curl`, `shasum` and `xxd`.

```sh
go test ./...                              # unit tests, on macOS or Linux
GOOS=linux GOARCH=arm64 go vet ./...       # Linux-only code
./build.sh                                 # tests, launcher, image and Swift package
```

`build.sh` writes these files to `stage/`:

- `cs-project-image.tar.gz`: a gzipped OCI layout archive of the image.
- `swift/`: a Swift package, `CSProjectImage`, that has the archive as a C array and its SHA-256.

## Pinned components

`build.sh` downloads these files, checks each SHA-256, and puts them in the image:

| Component | Version | File |
|---|---|---|
| runc | 1.5.3 | `runc.arm64` |
| CNI plugins | 1.10.0 | `cni-plugins-linux-arm64-v1.10.0.tgz` (`bridge`, `loopback`, `static`) |

The image therefore contains runc and the CNI plugins. Both are Apache-2.0.

## License

Apache-2.0. See [LICENSE](LICENSE).
