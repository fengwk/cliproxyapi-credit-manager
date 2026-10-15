package plugin

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// A failed swap cannot resume a probe loop that was already quiesced.
func TestWarmupSwapTimeoutPreservesQuiescedPause(t *testing.T) {
	fake := newFakeHost()
	fake.hold()
	h := newWarmupHarness(t, fake, warmupEnabledTwo)
	awaitProbe(t, fake)
	if _, err := h.m.HandleCall(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	oldGrace := warmupSwapGrace
	warmupSwapGrace = time.Millisecond
	t.Cleanup(func() { warmupSwapGrace = oldGrace })
	envelope := reconfigureRaw(t, h.m, pluginabi.MethodPluginReconfigure, warmupYAML(h.dir, warmupDisabledBlock))
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusConflict {
		t.Fatalf("expected busy: %+v", envelope)
	}
	worker := h.m.warmupRunning()
	worker.mu.Lock()
	paused := worker.paused
	worker.mu.Unlock()
	fake.release()
	awaitPassComplete(t, worker)
	if !paused || stateOf(h.m) != stateQuiesced || len(fake.recorded()) != 1 {
		t.Fatalf("failed swap resumed quiesced probes: paused=%v calls=%d", paused, len(fake.recorded()))
	}
}
