package client

import (
	"testing"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

func TestLiveKitConnectOptions(t *testing.T) {
	c := &liveKit{nack: &nackGenSet{}}
	opts, err := c.connectOptions()
	if err != nil {
		t.Fatal(err)
	}
	p := lksdk.ApplyConnectOptions(opts...)
	if g, ok := p.NackGenerator.(*nackGenSet); !ok || g != c.nack {
		t.Fatalf("NackGenerator = %T %p, want the participant's nackGenSet", p.NackGenerator, p.NackGenerator)
	}
	if !p.IncludeDefaultInterceptors {
		t.Fatal("default interceptors off: the NACK generator would not be registered")
	}
	if len(p.Interceptors) != 1 || !p.AutoSubscribe {
		t.Fatalf("interceptors %d, auto-subscribe %v", len(p.Interceptors), p.AutoSubscribe)
	}
}
