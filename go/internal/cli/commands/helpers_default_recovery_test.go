package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestDefaultDeviceRecoveryRequiresAffirmativeChoiceBeforePicker(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	var question string
	confirmDefaultRecoveryFn = func(q string) (bool, error) {
		question = q
		return false, nil // Enter or No.
	}
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("picker opened without affirmative confirmation")
		return nil, nil
	}

	cause := errors.New("unreachable")
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, cause, nil, false, false, false, nil)
	var stopped *defaultDeviceRecoveryStoppedError
	if selected != nil || !errors.As(err, &stopped) || !errors.Is(err, cause) {
		t.Fatalf("declined recovery = (%v, %v), want no selection and original failure", selected, err)
	}
	if errors.Is(err, ErrUserCancelled) {
		t.Fatalf("declined recovery was reported as a successful cancellation: %v", err)
	}
	if question != "Default device \"wendyos-old.local\" is unreachable after 1.00 second. Pick a different device for this command?" {
		t.Fatalf("confirmation question = %q", question)
	}
}

func TestDefaultDeviceRecoveryOpensPickerAfterAffirmativeChoice(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	confirmations := 0
	confirmDefaultRecoveryFn = func(string) (bool, error) {
		confirmations++
		return true, nil
	}
	excluded := map[string]bool{"docker": true}
	want := &SelectedDevice{PinKey: "wendyos-new.local"}
	pickerCalls := 0
	pickDefaultRecoveryDeviceFn = func(_ context.Context, gotExcluded map[string]bool, bluetooth, suppressUpdate, disableEnroll bool) (*SelectedDevice, error) {
		pickerCalls++
		if !gotExcluded["docker"] || !bluetooth || !suppressUpdate || !disableEnroll {
			t.Fatalf("picker options changed: excluded=%v bluetooth=%v suppressUpdate=%v disableEnroll=%v", gotExcluded, bluetooth, suppressUpdate, disableEnroll)
		}
		return want, nil
	}

	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", 2*time.Second, errors.New("unreachable"), excluded, true, true, true, nil)
	if err != nil || selected != want {
		t.Fatalf("accepted recovery = (%v, %v), want selected device", selected, err)
	}
	if confirmations != 1 || pickerCalls != 1 {
		t.Fatalf("confirmations=%d picker calls=%d, want one each", confirmations, pickerCalls)
	}
}

func TestDefaultDeviceRecoveryCancellationSkipsPicker(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	confirmDefaultRecoveryFn = func(string) (bool, error) { return false, tui.ErrCancelled }
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("picker opened after cancellation")
		return nil, nil
	}
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, errors.New("unreachable"), nil, false, false, false, nil)
	if selected != nil || !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancelled recovery = (%v, %v), want ErrUserCancelled", selected, err)
	}
}

func TestDefaultDeviceRecoveryDoesNotPromptAfterContextCancellation(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	confirmDefaultRecoveryFn = func(string) (bool, error) {
		t.Fatal("confirmation opened after context cancellation")
		return false, nil
	}
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("picker opened after context cancellation")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	selected, err := handleDefaultDeviceRecovery(ctx, "wendyos-old.local", time.Second, context.Canceled, nil, false, false, false, nil)
	if selected != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context = (%v, %v), want context.Canceled", selected, err)
	}
}

func TestDefaultDeviceRecoveryPromptFailureStopsSelection(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	cause := errors.New("terminal unavailable")
	confirmDefaultRecoveryFn = func(string) (bool, error) { return false, cause }
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("picker opened after confirmation failed")
		return nil, nil
	}
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, errors.New("unreachable"), nil, false, false, false, nil)
	var stopped *defaultDeviceRecoveryStoppedError
	if selected != nil || !errors.As(err, &stopped) || !errors.Is(err, cause) {
		t.Fatalf("failed prompt = (%v, %v), want terminal failure without selection", selected, err)
	}
}

func TestDefaultDeviceRecoveryTriesSameTargetBeforePicker(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	confirmDefaultRecoveryFn = func(string) (bool, error) {
		t.Fatal("prompt opened although the default was reached through the fallback")
		return false, nil
	}
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("picker opened although the default was reached through the fallback")
		return nil, nil
	}
	want := &SelectedDevice{PinKey: "cloud-default"}
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, errors.New("LAN unavailable"), nil, false, false, false,
		func(_ context.Context, name string) (*SelectedDevice, error) {
			if name != "wendyos-old.local" {
				t.Fatalf("fallback target = %q, want saved default", name)
			}
			return want, nil
		})
	if err != nil || selected != want {
		t.Fatalf("same-target fallback = (%v, %v), want selected default", selected, err)
	}
}

func TestDefaultDeviceRecoveryPromptsAfterSameTargetFallbackFails(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})

	order := []string{}
	confirmDefaultRecoveryFn = func(string) (bool, error) {
		order = append(order, "confirm")
		return true, nil
	}
	want := &SelectedDevice{PinKey: "other-device"}
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		order = append(order, "picker")
		return want, nil
	}
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, errors.New("LAN unavailable"), nil, false, false, false,
		func(context.Context, string) (*SelectedDevice, error) {
			order = append(order, "same target")
			return nil, errors.New("Cloud unavailable")
		})
	if err != nil || selected != want {
		t.Fatalf("recovery after fallback = (%v, %v), want selected alternate device", selected, err)
	}
	if got := strings.Join(order, ","); got != "same target,confirm,picker" {
		t.Fatalf("recovery order = %q", got)
	}
}

func TestDefaultDeviceRecoverySurfacesCloudIdentityMismatch(t *testing.T) {
	oldConfirm := confirmDefaultRecoveryFn
	oldPicker := pickDefaultRecoveryDeviceFn
	t.Cleanup(func() {
		confirmDefaultRecoveryFn = oldConfirm
		pickDefaultRecoveryDeviceFn = oldPicker
	})
	confirmDefaultRecoveryFn = func(string) (bool, error) {
		t.Fatal("identity mismatch was hidden by the picker prompt")
		return false, nil
	}
	pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("identity mismatch was hidden by the picker")
		return nil, nil
	}
	selected, err := handleDefaultDeviceRecovery(context.Background(), "wendyos-old.local", time.Second, errors.New("LAN unavailable"), nil, false, false, false,
		func(context.Context, string) (*SelectedDevice, error) {
			return nil, refuseIdentity("Cloud asset has a different identity")
		})
	if selected != nil || !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("identity mismatch = (%v, %v), want typed refusal", selected, err)
	}
}
