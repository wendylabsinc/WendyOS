package appv1

import (
	"reflect"
	"testing"
	"time"
)

func TestClientEncodePreservesTypedCaptureExactly(t *testing.T) {
	original, e := DecodePublish(requestFixture())
	if e != nil {
		t.Fatal(e)
	}
	wire, e := EncodePublish(original)
	if e != nil {
		t.Fatal(e)
	}
	recovered, e := DecodePublish(wire)
	if e != nil || !reflect.DeepEqual(original, recovered) {
		t.Fatal("client changed publication", e)
	}
	wire.PayloadJson[0] = '!'
	if reflect.DeepEqual(wire.PayloadJson, original.Payload) {
		t.Fatal("client data aliases payload")
	}
	original.Timing.SampleTime = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, e = EncodePublish(original); e == nil {
		t.Fatal("timestamp overflow accepted")
	}
}
