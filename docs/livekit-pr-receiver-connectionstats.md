**Title:** Guard receiver tracker callbacks until construction completes

## Problem

`NewWebRTCReceiver` calls `NewReceiverBase` first. That creates the `StreamTrackerManager`, which for video tracks immediately starts `go s.bitrateReporter()` (`streamtrackermanager.go:166`). Every second, the reporter calls the receiver's `StreamTrackerManagerListener` methods: `OnBitrateReport`, and `OnMaxTemporalLayerSeenChanged` through `updateMaxTemporalLayerSeen`. Those methods use both `w.ReceiverBase` and `w.connectionStats`:

```go
w.connectionStats.AddLayerTransition(w.ReceiverBase.StreamTrackerManager().DistanceToDesired())
```

Neither field is assigned until after `NewReceiverBase` returns. If the constructing goroutine doesn't run for more than the reporter's first 1 s tick, the reporter finds the fields still nil and the whole server panics:

```
panic: runtime error: invalid memory address or nil pointer dereference
github.com/livekit/livekit-server/pkg/sfu/connectionquality.(*ConnectionStats).AddLayerTransition
	pkg/sfu/connectionquality/connectionstats.go:193
github.com/livekit/livekit-server/pkg/sfu.(*WebRTCReceiver).OnBitrateReport
	pkg/sfu/receiver.go:285
github.com/livekit/livekit-server/pkg/sfu.(*ReceiverBase).OnBitrateReport
	pkg/sfu/receiver_base.go:598
github.com/livekit/livekit-server/pkg/sfu.(*StreamTrackerManager).bitrateReporter
	pkg/sfu/streamtrackermanager.go:654
```

We hit this twice in load tests, on v1.13.11 and on a fork based on it, and the same code is in v1.13.2 and current master. In each case thousands of rooms of 2 participants each were joining in a burst, with both participants publishing simulcast video. At the moment of the crash the server had ~7000 participants, ~73 busy cores and ~800k goroutines, and it was already logging `publish time out`. The process was not out of memory.

## Fix

- Add an `isReady` flag (`atomic.Bool`) to `WebRTCReceiver`. It is stored once `ReceiverBase` and `connectionStats` are both assigned. That is still before `connectionStats.Start()` and `UpdateTrackInfo`, so nothing changes once construction finishes.
- `OnAvailableLayersChanged`, `OnMaxPublishedLayerChanged`, `OnMaxTemporalLayerSeenChanged` and `OnBitrateReport` return early until the flag is set.

Why a flag instead of changing the order or checking for nil:

- **Reordering can't cover `ReceiverBase`.** `connectionStats` could be created before `NewReceiverBase`, but `w.ReceiverBase` is that call's return value, and the call has already started the reporter.
- **A plain nil check would still be a data race.** It would read fields that another goroutine writes without synchronization. The atomic store after both assignments, paired with the load in the callbacks, gives the reporter goroutine a happens-before edge on both fields.
- **Starting the reporter later would touch shared code.** It would mean changing `StreamTrackerManager`'s API and every caller. The flag keeps the change inside `WebRTCReceiver`.

The only cost is that callbacks arriving during construction are ignored. They only add a layer-transition sample to the quality score, and the next report comes a second later.

## Testing

- New test `TestWebRTCReceiver_TrackerCallbacksBeforeReady` calls the listener methods on an unconstructed receiver. It panics without the fix and passes with it.
- `go test -race ./pkg/sfu/ ./pkg/sfu/connectionquality/` passes.
