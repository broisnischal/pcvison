package bench

import (
	"fmt"
	"os"
	"testing"
	"time"

	"pc/internal/hypr"
	"pc/internal/sys"
)

func timeit(name string, n int, f func()) {
	t0 := time.Now()
	for i := 0; i < n; i++ {
		f()
	}
	fmt.Printf("%-22s %6.2fms\n", name, float64(time.Since(t0).Microseconds())/1000/float64(n))
}

func TestIPCCosts(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	hypr.Lua()
	timeit("monitors", 20, func() { hypr.Monitors() })
	timeit("clients", 20, func() { hypr.Clients() })
	timeit("activewindow", 20, func() { hypr.ActiveWindow() })
	timeit("cursorpos", 20, func() { hypr.CursorPos() })
	timeit("getoption follow_mouse", 20, func() { hypr.FollowMouse() })
	timeit("eval set follow_mouse", 10, func() { hypr.SetFollowMouse(1) })
	timeit("held modifiers", 20, func() { sys.HeldModifiers() })
}
