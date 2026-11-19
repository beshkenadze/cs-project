package caps

import (
	"reflect"
	"testing"
)

func TestDefaultSetIsDockerDefault(t *testing.T) {
	got, err := Effective(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL",
		"CAP_MKNOD", "CAP_NET_BIND_SERVICE", "CAP_NET_RAW", "CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP",
		"CAP_SETUID", "CAP_SYS_CHROOT",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestAllowedAdditionsAndDrops(t *testing.T) {
	got, err := Effective([]string{"ipc_lock", "CAP_SYS_NICE"}, []string{"MKNOD", "net_raw"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"CAP_IPC_LOCK", "CAP_SYS_NICE"} {
		if !has(got, c) {
			t.Fatalf("%s missing from %v", c, got)
		}
	}
	for _, c := range []string{"CAP_MKNOD", "CAP_NET_RAW"} {
		if has(got, c) {
			t.Fatalf("%s not dropped from %v", c, got)
		}
	}
}

func TestDropAll(t *testing.T) {
	got, err := Effective(nil, []string{"ALL"})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestRefusedAdditions(t *testing.T) {
	for _, name := range []string{"SYS_ADMIN", "CAP_SYS_PTRACE", "sys_module", "NET_ADMIN", "SYS_TIME", "SYS_RAWIO", "ALL", "NOPE"} {
		_, err := Effective([]string{name}, nil)
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
