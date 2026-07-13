package presence

import (
	"strconv"
	"sync"
	"testing"
)

// TestVisibleEmptyRegistry covers the base case: nothing has ever connected,
// so any hash — including one that looks plausible — must read as absent.
// This is the state every server restart resets to, so it has to be safe.
func TestVisibleEmptyRegistry(t *testing.T) {
	r := NewRegistry()
	if r.Visible("device-1") {
		t.Fatal("Visible on an empty registry must be false")
	}
}

// TestConnectUpdateVisible walks the happy path: a connection registers,
// reports itself visible, and Visible flips true for that hash; when the
// same connection later reports hidden, Visible flips back false.
func TestConnectUpdateVisible(t *testing.T) {
	r := NewRegistry()
	r.Connect("conn-1")

	r.Update("conn-1", "device-1", true)
	if !r.Visible("device-1") {
		t.Fatal("Visible should be true after Update(visible=true)")
	}

	r.Update("conn-1", "device-1", false)
	if r.Visible("device-1") {
		t.Fatal("Visible should be false after Update(visible=false)")
	}
}

// TestDisconnectRemoves confirms Disconnect deletes the entry outright
// (not just marks it hidden): both Visible and Connected must reflect that
// the connection is entirely gone, since that's what makes "not present"
// free and instant on stream close.
func TestDisconnectRemoves(t *testing.T) {
	r := NewRegistry()
	r.Connect("conn-1")
	r.Update("conn-1", "device-1", true)

	r.Disconnect("conn-1")

	if r.Visible("device-1") {
		t.Fatal("Visible should be false after Disconnect")
	}
	if r.Connected("conn-1") {
		t.Fatal("Connected should be false after Disconnect")
	}
}

// TestUpdateOnUnknownConnIsNoop is the load-bearing guard: a beacon for a
// connection the registry never Connected (or already Disconnected) must not
// create or resurrect an entry. Otherwise a stray/delayed beacon could mark
// a device visible forever with no live stream left to ever clear it.
func TestUpdateOnUnknownConnIsNoop(t *testing.T) {
	r := NewRegistry()
	r.Update("ghost", "device-1", true)

	if r.Visible("device-1") {
		t.Fatal("Update on a never-connected conn must not make it visible")
	}
	if r.Connected("ghost") {
		t.Fatal("Update on a never-connected conn must not create an entry")
	}
}

// TestReconnectResetsEntry checks that Connect on a re-used conn id
// overwrites whatever was there with a fresh zero entry, rather than
// preserving a prior tab's device/visibility across the reconnect.
func TestReconnectResetsEntry(t *testing.T) {
	r := NewRegistry()
	r.Connect("conn-1")
	r.Update("conn-1", "device-1", true)
	if !r.Visible("device-1") {
		t.Fatal("setup: expected visible after first Update")
	}

	r.Connect("conn-1") // reconnect with the same id

	if r.Visible("device-1") {
		t.Fatal("Connect must reset to a fresh entry, not carry forward visibility")
	}
	if !r.Connected("conn-1") {
		t.Fatal("Connect must leave the conn registered")
	}

	// A fresh Update after reconnect works normally.
	r.Update("conn-1", "device-1", true)
	if !r.Visible("device-1") {
		t.Fatal("Update after reconnect should be recorded")
	}
}

// TestMultiTabAnyVisible exercises the "any" in Visible's contract: a device
// with two tabs is present as long as at least one is visible, and stops
// being present only once the visible one disconnects (leaving only the
// hidden one alive).
func TestMultiTabAnyVisible(t *testing.T) {
	r := NewRegistry()
	r.Connect("tab-a")
	r.Connect("tab-b")
	r.Update("tab-a", "device-1", false) // hidden
	r.Update("tab-b", "device-1", true)  // visible

	if !r.Visible("device-1") {
		t.Fatal("device should be visible: one of two tabs is visible")
	}

	r.Disconnect("tab-b") // the visible tab closes; tab-a (hidden) remains

	if r.Visible("device-1") {
		t.Fatal("device should no longer be visible: only a hidden tab remains")
	}
}

// TestEmptyHashNeverVisible checks the guard against matching on an
// under-initialized connection's zero-value/empty deviceHash: Update with an
// explicit empty hash and visible=true must never make Visible("") true.
func TestEmptyHashNeverVisible(t *testing.T) {
	r := NewRegistry()
	r.Connect("conn-1")
	r.Update("conn-1", "", true)

	if r.Visible("") {
		t.Fatal(`Visible("") must always be false, even after Update with an empty hash`)
	}
}

// TestConcurrent is a smoke test under -race: several goroutines hammer
// Connect/Update/Visible/Disconnect/Connected concurrently on overlapping
// conn ids. It asserts no crash/deadlock and no data race; it does not
// assert specific outcomes, since the interleaving is nondeterministic by
// design.
func TestConcurrent(t *testing.T) {
	r := NewRegistry()
	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			conn := "conn-" + strconv.Itoa(g%3) // overlap ids across goroutines
			device := "device-" + strconv.Itoa(g%2)
			for i := 0; i < iterations; i++ {
				switch i % 4 {
				case 0:
					r.Connect(conn)
				case 1:
					r.Update(conn, device, i%2 == 0)
				case 2:
					_ = r.Visible(device)
				case 3:
					_ = r.Connected(conn)
				}
			}
			r.Disconnect(conn)
		}(g)
	}
	wg.Wait()
}
