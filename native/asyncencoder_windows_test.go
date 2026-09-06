//go:build windows

package native

import (
	"testing"
	"time"
)

// TestAsyncMFEncoder 用同步 Encode 契约逐帧编码，验证封装器正确返回逐帧 H.264。
func TestAsyncMFEncoder(t *testing.T) {
	enc, err := NewAsyncMFH264Encoder(1280, 720, 4_000_000)
	if err != nil {
		t.Fatalf("NewAsyncMFH264Encoder 失败: %v", err)
	}
	defer enc.Close()

	n := 30
	t0 := time.Now()
	var first []byte
	nonEmpty := 0
	for i := 0; i < n; i++ {
		nv12 := makeNV12Gradient(1280, 720, i)
		out, err := enc.Encode(nv12)
		if err != nil {
			t.Fatalf("第%d帧 Encode 失败: %v", i, err)
		}
		if len(out) == 0 {
			t.Fatalf("第%d帧输出为空", i)
		}
		nonEmpty++
		if i == 0 {
			first = out
		}
	}
	el := time.Since(t0)
	fps := float64(nonEmpty) / el.Seconds()
	t.Logf("AsyncMFEncoder 同步逐帧 %d 帧 耗时 %.2fs → %.1f fps", nonEmpty, el.Seconds(), fps)

	if ok, detail := validateAnnexB(first); !ok {
		t.Fatalf("首帧非合法 H.264(IDR/SPS/PPS): %s", detail)
	} else {
		t.Log("首帧含 IDR+SPS+PPS，输出合法")
	}
}
