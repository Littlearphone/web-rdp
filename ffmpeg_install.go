package main

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ffmpegLocalDir = "ffmpeg_local"
const ffmpegReleaseAPI = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/latest"

var (
	ffmpegPath   string
	hasDDAGrab   bool
	useFFmpeg    bool
	h264Encoders []string // 可用编码器列表（GPU优先，CPU回退）
	h264EncIdx   int      // 当前使用的编码器索引
)

// 返回当前选中的 H.264 编码器名称
func currentH264Encoder() string {
	if h264EncIdx < len(h264Encoders) {
		return h264Encoders[h264EncIdx]
	}
	return ""
}

// 编码器失败时切换到下一个可用编码器，返回是否还有可用选项
func tryNextH264Encoder() bool {
	h264EncIdx++
	if h264EncIdx < len(h264Encoders) {
		log.Printf("H.264 编码器回退 → %s", h264Encoders[h264EncIdx])
		return true
	}
	return false
}

// detectFFmpeg 自动检测系统中的 ffmpeg 可执行文件。
// 优先级：本地目录 ffmpeg_local/ → PATH 环境变量 → 在线下载
// 下载的 BtbN 构建（FFmpeg ≥6.0）自带 ddagrab 滤镜（DXGI Desktop Duplication）。
func detectFFmpeg() {
	local := filepath.Join(ffmpegLocalDir, "bin", "ffmpeg.exe")

	// 1) 检查本地 ffmpeg_local/ 目录
	if _, err := os.Stat(local); err == nil {
		ffmpegPath = local
		hasDDAGrab = checkDDAGrab(local)
		useFFmpeg = true
		if !hasDDAGrab {
			if askYN("本地 ffmpeg 不支持 ddagrab（DXGI 桌面捕获），下载新版本？") {
				downloadAndExtract()
				if _, err := os.Stat(local); err == nil {
					ffmpegPath = local
					hasDDAGrab = checkDDAGrab(local)
				}
			}
			if !hasDDAGrab {
				log.Printf("→ 使用本地 ffmpeg（gdigrab 模式）")
			}
		}
		return
	}

	// 2) 检查 PATH 中的 ffmpeg
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpegPath = p
		hasDDAGrab = checkDDAGrab(p)
		useFFmpeg = true
		if !hasDDAGrab {
			if askYN("系统 ffmpeg 不支持 ddagrab（DXGI 桌面捕获），下载新版本？") {
				downloadAndExtract()
				if _, err := os.Stat(local); err == nil {
					ffmpegPath = local
					hasDDAGrab = checkDDAGrab(local)
				}
			}
			if !hasDDAGrab {
				log.Printf("→ 使用系统 ffmpeg（gdigrab 模式）")
			}
		}
		return
	}

	// 3) 系统中完全找不到 ffmpeg → 提示下载
	if askYN("未找到 ffmpeg，自动下载？") {
		downloadAndExtract()
		if _, err := os.Stat(local); err == nil {
			ffmpegPath = local
			hasDDAGrab = checkDDAGrab(local)
			useFFmpeg = true
			return
		}
	}

	// 最终回退：纯 Go 截图方案
	useFFmpeg = false
	fmt.Println("→ 使用纯 Go 截图方案（无 ffmpeg）")
}

// 通过 wmic 检测主显卡品牌，避免盲目尝试不兼容的 GPU 编码器
func detectGPUVendor() string {
	out, err := exec.Command("wmic", "path", "win32_VideoController", "get", "name").Output()
	if err != nil {
		return ""
	}
	s := strings.ToLower(string(out))
	if strings.Contains(s, "nvidia") {
		return "nvidia"
	}
	if strings.Contains(s, "amd") || strings.Contains(s, "radeon") {
		return "amd"
	}
	if strings.Contains(s, "intel") {
		return "intel"
	}
	return ""
}

// 按 GPU 品牌 + ffmpeg 编码器可用性构建编码器列表。
// 优先级：GPU 硬件编码器 → libx264（最终回退）。
// libx264 始终放在列表末尾，确保所有硬件编码器失败后直接使用 CPU 编码。
// 注意：h264_mf 不加入回退链（无论 gdigrab/ddagrab 均不可靠）。
func detectH264Encoder() {
	if len(h264Encoders) > 0 {
		return
	}
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return
	}
	s := string(out)
	vendor := detectGPUVendor()
	// 只添加匹配当前 GPU 品牌的硬件编码器
	switch vendor {
	case "nvidia":
		if strings.Contains(s, "h264_nvenc") {
			h264Encoders = append(h264Encoders, "h264_nvenc")
		}
	case "amd":
		if strings.Contains(s, "h264_amf") {
			h264Encoders = append(h264Encoders, "h264_amf")
		}
	case "intel":
		if strings.Contains(s, "h264_qsv") {
			h264Encoders = append(h264Encoders, "h264_qsv")
		}
	}
	// CPU 软件编码 libx264（画质最好、兼容性最广，作为最终回退，永不跳过）。
	// 始终放在列表末尾；若 libx264 也失败，tryNextH264Encoder() 返回 false，系统回退到 MJPEG。
	if strings.Contains(s, "libx264") {
		h264Encoders = append(h264Encoders, "libx264")
	}
	if len(h264Encoders) > 0 {
		log.Printf("GPU: %s → H.264 编码器: %s (共 %d 个)", vendor, h264Encoders[0], len(h264Encoders))
	} else {
		log.Printf("GPU: %s → 未找到可用 H.264 编码器", vendor)
	}
}

// askYN 在控制台向用户询问 Y/n 问题，返回用户的选择。
// 直接回车、y、yes 视为确认；其他输入视为拒绝。
func askYN(prompt string) bool {
	fmt.Printf("\n⚠ %s [Y/n]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "" || line == "y" || line == "yes"
}

// resolveDownloadURL 从 GitHub Releases API 获取最新 win64-gpl ffmpeg 压缩包的下载地址
func resolveDownloadURL() string {
	resp, err := httpClient.Get(ffmpegReleaseAPI)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&release) != nil {
		return ""
	}
	for _, a := range release.Assets {
		if strings.Contains(a.Name, "win64") && strings.Contains(a.Name, "gpl") && strings.HasSuffix(a.Name, ".zip") {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// checkDDAGrab 检查指定 ffmpeg 是否支持 ddagrab 滤镜（DXGI 桌面捕获）。
// ddagrab 是 FFmpeg 6.0+ 内置的 video source filter，底层使用 Windows Desktop Duplication API。
func checkDDAGrab(path string) bool {
	out, err := exec.Command(path, "-hide_banner", "-filters").Output()
	return err == nil && strings.Contains(string(out), "ddagrab")
}

// downloadAndExtract 下载 ffmpeg 压缩包并解压到 ffmpeg_local/ 目录。
// 包含下载进度条、速度估算和断点续传提示（通过重试机制）。
// 解压时会自动去除压缩包顶层目录，将 bin/ 等子目录直接放入 ffmpeg_local/。
func downloadAndExtract() {
	tmpFile := filepath.Join(os.TempDir(), "ffmpeg_download.zip")
	defer func() { _ = os.Remove(tmpFile) }()
	for attempt := 1; ; attempt++ {
		dlURL := resolveDownloadURL()
		if dlURL == "" {
			fmt.Println("  无法获取下载地址")
			fmt.Println("  请前往: https://github.com/BtbN/FFmpeg-Builds/releases")
			fmt.Println("  选择 win64-gpl.zip 下载到本地，解压后使用 -ffmpeg 指定路径")
			fmt.Println("  或在命令提示符中执行: winget install ffmpeg")
			return
		}
		fmt.Printf("下载 ffmpeg... (约 120MB)\n")
		resp, err := httpClient.Get(dlURL)
		if err != nil {
			fmt.Printf("  下载失败: %v\n", err)
			if askYN("重试下载？") {
				continue
			}
			return
		}
		f, _ := os.Create(tmpFile)
		totalSize, downloaded, startTime := resp.ContentLength, int64(0), time.Now()
		buf, lastReport := make([]byte, 64*1024), time.Now()
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = f.Write(buf[:n])
				downloaded += int64(n)
				if totalSize > 0 && time.Since(lastReport) > 200*time.Millisecond {
					elapsed := time.Since(startTime).Seconds()
					speed := float64(downloaded) / elapsed / 1024 / 1024
					pct := downloaded * 100 / totalSize
					eta := ""
					if speed > 0 {
						eta = fmt.Sprintf(" 剩余 %ds", int(float64(totalSize-downloaded)/(speed*1024*1024)))
					}
					fmt.Printf("\r  %d%%  %.1f MB/s%s    ", pct, speed, eta)
					lastReport = time.Now()
				} else if totalSize <= 0 {
					fmt.Printf("\r  已下载 %.1f MB    ", float64(downloaded)/1024/1024)
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = f.Close()
		_ = resp.Body.Close()
		if totalSize > 0 && downloaded < totalSize {
			fmt.Printf("\n  下载不完整\n")
			if askYN("重试下载？") {
				continue
			}
			return
		}
		fmt.Printf("\n  下载完成 (%.1f MB)\n", float64(downloaded)/1024/1024)
		fmt.Println("解压...")
		_ = os.RemoveAll(ffmpegLocalDir)
		zr, err := zip.OpenReader(tmpFile)
		if err != nil {
			fmt.Printf("  解压失败\n")
			if askYN("重试下载？") {
				continue
			}
			return
		}
		ok := true
		for _, zf := range zr.File {
			parts := strings.SplitN(zf.Name, "/", 2)
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			target := filepath.Join(ffmpegLocalDir, parts[1])
			if zf.FileInfo().IsDir() {
				_ = os.MkdirAll(target, 0755)
				continue
			}
			_ = os.MkdirAll(filepath.Dir(target), 0755)
			rc, e1 := zf.Open()
			out, e2 := os.Create(target)
			if e1 != nil || e2 != nil {
				ok = false
				break
			}
			_, _ = io.Copy(out, rc)
			_ = rc.Close()
			_ = out.Close()
		}
		_ = zr.Close()
		if !ok {
			fmt.Println("  解压失败")
			_ = os.RemoveAll(ffmpegLocalDir)
			if askYN("重试下载？") {
				continue
			}
			return
		}
		fmt.Printf("→ ffmpeg 就绪 (ffmpeg_local/)  重试 %d 次\n", attempt-1)
		return
	}
}
