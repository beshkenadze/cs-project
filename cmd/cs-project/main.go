//go:build linux

// cs-project is the launcher of a project VM. `cs-project init` is the init process of the VM
// container. Each other command is one exec from socktainer, with JSON on stdin and stdout.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/beshkenadze/cs-project/internal/daemon"
	"github.com/beshkenadze/cs-project/internal/launcher"
	"github.com/beshkenadze/cs-project/internal/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cs-project:", err)
		os.Exit(1)
	}
}

func readJSON(v any) error { return json.NewDecoder(os.Stdin).Decode(v) }

func writeJSON(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }

func usage() error {
	return fmt.Errorf("usage: cs-project init|version|create|start <id>|kill <id> <signal>|cleanup <id>|remove|boot|hosts|events")
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	l := &launcher.Launcher{
		Sys: launcher.LinuxSystem{}, Data: protocol.DataDir, Host: protocol.HostDir, State: protocol.StateDir,
		ResolvSource: "/etc/resolv.conf",
	}
	switch args[0] {
	case "init":
		return daemon.RunInit()
	case "version":
		return writeJSON(map[string]int{"protocol": protocol.Version})
	case "create":
		var m protocol.Member
		if err := readJSON(&m); err != nil {
			return err
		}
		return l.Create(m)
	case "start":
		if len(args) != 2 {
			return usage()
		}
		pid, err := l.Start(args[1])
		if err != nil {
			return err
		}
		return writeJSON(map[string]int{"pid": pid})
	case "kill":
		if len(args) != 3 {
			return usage()
		}
		return l.Kill(args[1], args[2])
	case "cleanup":
		if len(args) != 2 {
			return usage()
		}
		return l.Cleanup(args[1])
	case "remove":
		var r protocol.Remove
		if err := readJSON(&r); err != nil {
			return err
		}
		return l.Remove(r)
	case "boot":
		var b protocol.Boot
		if err := readJSON(&b); err != nil {
			return err
		}
		return l.Boot(b)
	case "hosts":
		var content map[string]string
		if err := readJSON(&content); err != nil {
			return err
		}
		return l.WriteHosts(content)
	case "events":
		conn, err := net.Dial("unix", protocol.ControlSock)
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := json.NewEncoder(conn).Encode(protocol.Request{Op: "subscribe"}); err != nil {
			return err
		}
		_, err = io.Copy(os.Stdout, conn)
		return err
	}
	return usage()
}
