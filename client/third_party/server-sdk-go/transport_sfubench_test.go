package lksdk

import (
	"sync/atomic"
	"testing"

	protoLogger "github.com/livekit/protocol/logger"
	"github.com/pion/interceptor"
)

type countingFactory struct{ calls atomic.Int32 }

func (f *countingFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	f.calls.Add(1)
	return &interceptor.NoOp{}, nil
}

func TestPCTransportNackGenerator(t *testing.T) {
	f := &countingFactory{}
	tr, err := NewPCTransport(PCTransportParams{IncludeDefaultInterceptors: true, NackGenerator: f})
	if err != nil {
		t.Fatal(err)
	}
	tr.SetLogger(protoLogger.GetLogger())
	defer tr.Close()
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("NackGenerator.NewInterceptor called %d times, want 1", n)
	}
	if tr.nackGenerator != nil {
		t.Fatal("SDK NACK generator registered next to the replacement")
	}

	def, err := NewPCTransport(PCTransportParams{IncludeDefaultInterceptors: true})
	if err != nil {
		t.Fatal(err)
	}
	def.SetLogger(protoLogger.GetLogger())
	defer def.Close()
	if def.nackGenerator == nil {
		t.Fatal("SDK NACK generator missing without a replacement")
	}
}
