package services

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func jetsonEFIPayload(attributes, value uint32) []byte {
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[:4], attributes)
	binary.LittleEndian.PutUint32(raw[4:], value)
	return raw
}

type jetsonPreparationFixture struct {
	p      jetsonRootfsPreparation
	writes []string
}

func newJetsonPreparationFixture(t *testing.T) *jetsonPreparationFixture {
	t.Helper()
	f := &jetsonPreparationFixture{}
	f.p = jetsonRootfsPreparation{
		efivarsDir: t.TempDir(),
		appBPath:   filepath.Join(t.TempDir(), "APP_b"),
		lookPath:   func(string) (string, error) { return "/usr/sbin/nvbootctrl", nil },
		writeVar: func(path string, data []byte) error {
			f.writes = append(f.writes, filepath.Base(path))
			return os.WriteFile(path, data, 0o644)
		},
	}
	if err := os.WriteFile(f.p.appBPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *jetsonPreparationFixture) seed(t *testing.T, name string, attributes, value uint32) {
	t.Helper()
	if err := os.WriteFile(f.p.variablePath(name), jetsonEFIPayload(attributes, value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *jetsonPreparationFixture) wantVariable(t *testing.T, name string, attributes, value uint32) {
	t.Helper()
	raw, err := os.ReadFile(f.p.variablePath(name))
	if err != nil {
		t.Fatal(err)
	}
	if want := jetsonEFIPayload(attributes, value); !reflect.DeepEqual(raw, want) {
		t.Fatalf("%s = % x, want % x", name, raw, want)
	}
}

func TestJetsonRootfsPreparationEnablesMissingOrDisabledSettings(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "disabled"}[existing], func(t *testing.T) {
			f := newJetsonPreparationFixture(t)
			if existing {
				f.seed(t, rootfsRedundancy, 7, 0)
				f.seed(t, rootfsRetryMax, 7, 0)
			}
			changed, err := f.p.prepare(context.Background())
			if err != nil || !changed {
				t.Fatalf("prepare = %v, %v; want changed", changed, err)
			}
			f.wantVariable(t, rootfsRedundancy, 7, 1)
			f.wantVariable(t, rootfsRetryMax, 7, 3)
			wantWrites := []string{rootfsRetryMax + "-" + jetsonEFIGUID, rootfsRedundancy + "-" + jetsonEFIGUID}
			if !reflect.DeepEqual(f.writes, wantWrites) {
				t.Fatalf("write order = %v, want retries before redundancy: %v", f.writes, wantWrites)
			}
			f.writes = nil
			if changed, err := f.p.prepare(context.Background()); err != nil || changed || len(f.writes) != 0 {
				t.Fatalf("second preparation must be a no-op: changed=%v err=%v writes=%v", changed, err, f.writes)
			}
		})
	}
}

func TestJetsonRootfsPreparationPreservesFirmwareSettings(t *testing.T) {
	for _, attrs := range []uint32{6, 7} {
		t.Run(map[uint32]string{6: "volatile defaults", 7: "persistent"}[attrs], func(t *testing.T) {
			f := newJetsonPreparationFixture(t)
			f.seed(t, rootfsRedundancy, attrs, 1)
			f.seed(t, rootfsRetryMax, attrs, 2)
			if changed, err := f.p.prepare(context.Background()); err != nil || changed || len(f.writes) != 0 {
				t.Fatalf("configured board must be a no-op: changed=%v err=%v writes=%v", changed, err, f.writes)
			}
			f.wantVariable(t, rootfsRetryMax, attrs, 2)
		})
	}
	f := newJetsonPreparationFixture(t)
	f.seed(t, rootfsRetryMax, 6, 3) // NVIDIA's locked retry default must not be rewritten.
	if _, err := f.p.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.wantVariable(t, rootfsRetryMax, 6, 3)
	if len(f.writes) != 1 || f.writes[0] != rootfsRedundancy+"-"+jetsonEFIGUID {
		t.Fatalf("must only write redundancy: %v", f.writes)
	}
}

func TestJetsonRootfsPreparationRejectsUnsafeStateBeforeWriting(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *jetsonPreparationFixture)
		cause error
	}{
		{"missing alternate slot", func(t *testing.T, f *jetsonPreparationFixture) { f.p.appBPath += "-missing" }, os.ErrNotExist},
		{"missing EFI mount", func(t *testing.T, f *jetsonPreparationFixture) { f.p.efivarsDir += "-missing" }, os.ErrNotExist},
		{"EFI path is a file", func(t *testing.T, f *jetsonPreparationFixture) { f.p.efivarsDir = f.p.appBPath }, nil},
		{"volatile disabled level", func(t *testing.T, f *jetsonPreparationFixture) { f.seed(t, rootfsRedundancy, 6, 0) }, nil},
		{"volatile disabled retries", func(t *testing.T, f *jetsonPreparationFixture) { f.seed(t, rootfsRetryMax, 6, 0) }, nil},
		{"unsupported level", func(t *testing.T, f *jetsonPreparationFixture) { f.seed(t, rootfsRedundancy, 7, 2) }, nil},
		{"unsupported retry count", func(t *testing.T, f *jetsonPreparationFixture) { f.seed(t, rootfsRetryMax, 7, 4) }, nil},
		{"unsupported attributes", func(t *testing.T, f *jetsonPreparationFixture) { f.seed(t, rootfsRedundancy, 3, 0) }, nil},
		{"truncated variable", func(t *testing.T, f *jetsonPreparationFixture) {
			if err := os.WriteFile(f.p.variablePath(rootfsRedundancy), []byte{7}, 0o644); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"unreadable variable", func(t *testing.T, f *jetsonPreparationFixture) {
			if err := os.Mkdir(f.p.variablePath(rootfsRedundancy), 0o755); err != nil {
				t.Fatal(err)
			}
		}, syscall.EISDIR},
		{"broken tool lookup", func(t *testing.T, f *jetsonPreparationFixture) {
			f.p.lookPath = func(string) (string, error) { return "", os.ErrPermission }
		}, os.ErrPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newJetsonPreparationFixture(t)
			tt.setup(t, f)
			if _, err := f.p.prepare(context.Background()); err == nil || tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Fatalf("error = %v, want rejection with cause %v", err, tt.cause)
			}
			if len(f.writes) != 0 {
				t.Fatalf("unsafe state caused writes: %v", f.writes)
			}
		})
	}
}

func TestJetsonRootfsPreparationRequiresVerifiedWrites(t *testing.T) {
	tests := []struct {
		name  string
		write func(string, []byte) error
		cause error
	}{
		{"firmware rejects", func(string, []byte) error { return syscall.EINVAL }, syscall.EINVAL},
		{"permission denied", func(string, []byte) error { return os.ErrPermission }, os.ErrPermission},
		{"write silently ignored", func(string, []byte) error { return nil }, nil},
		{"firmware drops persistence", func(path string, data []byte) error {
			data[0] = 6
			return os.WriteFile(path, data, 0o644)
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newJetsonPreparationFixture(t)
			f.seed(t, rootfsRetryMax, 6, 3)
			f.p.writeVar = tt.write
			if _, err := f.p.prepare(context.Background()); err == nil || tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Fatalf("error = %v, want rejection with cause %v", err, tt.cause)
			}
		})
	}
}

func TestJetsonRootfsPreparationRetryFailureDoesNotEnableRedundancy(t *testing.T) {
	f := newJetsonPreparationFixture(t)
	f.p.writeVar = func(path string, data []byte) error {
		f.writes = append(f.writes, filepath.Base(path))
		return syscall.EPERM
	}
	if _, err := f.p.prepare(context.Background()); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("error = %v", err)
	}
	if len(f.writes) != 1 || f.writes[0] != rootfsRetryMax+"-"+jetsonEFIGUID {
		t.Fatalf("failed retry setup must not enable redundancy: %v", f.writes)
	}
}

func TestJetsonRootfsPreparationSkipsOtherBoardsAndCancellation(t *testing.T) {
	f := newJetsonPreparationFixture(t)
	f.p.lookPath = func(string) (string, error) { return "", &exec.Error{Name: "nvbootctrl", Err: exec.ErrNotFound} }
	f.p.efivarsDir += "-missing"
	f.p.appBPath += "-missing"
	if changed, err := f.p.prepare(context.Background()); err != nil || changed {
		t.Fatalf("non-Jetson: %v, %v", changed, err)
	}
	f = newJetsonPreparationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.p.prepare(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("cancelled preparation wrote variables: %v", f.writes)
	}

	ctx, cancel = context.WithCancel(context.Background())
	write := f.p.writeVar
	f.p.writeVar = func(path string, data []byte) error { cancel(); return write(path, data) }
	if _, err := f.p.prepare(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(f.writes) != 1 || f.writes[0] != rootfsRetryMax+"-"+jetsonEFIGUID {
		t.Fatalf("cancelled retry setup must not enable redundancy: %v", f.writes)
	}
}
