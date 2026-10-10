// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"strings"
	"testing"

	"github.com/go-widgets/painter"
	"github.com/go-widgets/toolkit"
)

const testW, testH = 1100, 640

// layout lays the tree out and draws it, as a window back-end does every
// frame -- which is also what drains the queue.
func layout(t *testing.T, v *view) []byte {
	t.Helper()
	buf := make([]byte, 4*testW*testH)
	v.root.SetBounds(toolkit.Rect{W: testW, H: testH})
	v.root.Draw(painter.NewPixelPainter(buf, testW, testH), toolkit.DefaultDark())
	return buf
}

// click presses the middle of w, through the root, as a back-end delivers
// a pointer event.
func click(v *view, w toolkit.Widget) {
	b := w.Bounds()
	ev := toolkit.Event{Kind: toolkit.EventClick, X: b.X + b.W/2, Y: b.Y + b.H/2}
	v.root.OnEvent(ev)
	ev.Kind = toolkit.EventMouseUp
	v.root.OnEvent(ev)
}

// The view shows what the ViewModel holds, and a click goes back to it.
func TestViewBindsAndDispatches(t *testing.T) {
	f := newFixture(t)
	v := newView(f.vm, f.q, nil)
	buf := layout(t, v)
	if strings.Join(v.servers.Items, "|") != "Paris|b  (other login)" {
		t.Fatalf("servers %q", v.servers.Items)
	}
	if strings.Join(v.shares.Items, "|") != "archive  /srv/archive|photos  /srv/photos" {
		t.Fatalf("shares %q", v.shares.Items)
	}
	if v.grants.Items[0] != "@staff (read)" || v.user.Text().Get() != "alice" {
		t.Fatalf("grants %q, user %q", v.grants.Items, v.user.Text().Get())
	}
	// A command that cannot run greys its button.
	if !v.create.Disabled().Get() || v.reload.Disabled().Get() || !v.enable.Disabled().Get() || v.disable.Disabled().Get() {
		t.Fatal("buttons do not follow their commands")
	}
	// Something was drawn: the frame is not one flat colour.
	if flat(buf) {
		t.Fatal("the frame is one colour")
	}

	// A click on Delete reaches the command: the first press arms it.
	click(v, v.del)
	f.settle()
	layout(t, v)
	if !strings.Contains(v.status.Text().Get(), "Press Delete again") {
		t.Fatalf("status %q", v.status.Text().Get())
	}
}

// A result posted from another goroutine is applied by the next frame's
// SetBounds, on the goroutine that draws -- and not before.
func TestTheFrameDrainsTheQueue(t *testing.T) {
	f := newFixture(t)
	v := newView(f.vm, f.q, nil)
	layout(t, v)
	done := make(chan struct{})
	go func() {
		f.q.Post(func() { f.vm.Status.Set("from a goroutine") })
		close(done)
	}()
	<-done
	if v.status.Text().Get() == "from a goroutine" {
		t.Fatal("applied before a frame")
	}
	layout(t, v)
	if v.status.Text().Get() != "from a goroutine" {
		t.Fatalf("status %q after a frame", v.status.Text().Get())
	}
}

func flat(buf []byte) bool {
	for i := 4; i < len(buf); i += 4 {
		if buf[i] != buf[0] || buf[i+1] != buf[1] || buf[i+2] != buf[2] {
			return false
		}
	}
	return true
}
