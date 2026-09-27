package menubar

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"shioriko/internal/mtpdev"
	"shioriko/internal/service"
)

// view is everything the Objective-C side needs to draw the status item and
// its menu. All wording lives here so the native code only lays it out.
type view struct {
	// Phase selects the status item icon: see service.Phase.String, plus
	// "starting" and "quitting".
	Phase    string `json:"phase"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	// Tone colours the header badge: "active", "idle", "warning".
	Tone string `json:"tone"`
	// Active makes the status item show the transfer icon.
	Active bool `json:"active"`

	MountPoint string        `json:"mountPoint,omitempty"`
	Storages   []storageView `json:"storages"`
	// Notes are small secondary lines under the header (transfer speed, hints).
	Notes []string `json:"notes"`

	CanReveal  bool   `json:"canReveal"`
	CanEject   bool   `json:"canEject"`
	EjectTitle string `json:"ejectTitle"`
	CanMount   bool   `json:"canMount"`
	MountTitle string `json:"mountTitle"`
	Paused     bool   `json:"paused"`
	CanPause   bool   `json:"canPause"`
	Tooltip    string `json:"tooltip"`
}

type storageView struct {
	Name     string  `json:"name"`
	Detail   string  `json:"detail"`
	Fraction float64 `json:"fraction"` // used / capacity, 0..1
	Warn     bool    `json:"warn"`     // nearly full
}

// quoted wraps a device name in Japanese corner brackets.
func quoted(name string) string {
	if name == "" {
		return "スマホ"
	}
	return "「" + name + "」"
}

// makeView turns a service status into menu content.
func makeView(st service.Status) view {
	v := view{Phase: st.Phase.String(), Tone: "idle", Paused: st.Paused, CanPause: true}
	name := quoted(st.Device)
	switch st.Phase {
	case service.Waiting:
		v.Title = "スマホを待っています"
		v.Subtitle = "USBの用途を「ファイル転送」にしてください"
		v.Tooltip = "Shioriko — 接続待ち"
	case service.Paused:
		v.Title = "自動マウントを一時停止中"
		v.Subtitle = "写真やイメージキャプチャからスマホを使えます"
		v.Tooltip = "Shioriko — 一時停止中"
	case service.Busy:
		v.Title = "スマホが他のアプリで使用中です"
		v.Subtitle = "adb などのアプリを終了してください"
		v.Tone = "warning"
		v.CanMount, v.MountTitle = true, "今すぐ再試行"
		v.Tooltip = "Shioriko — スマホが使用中"
	case service.Connecting:
		if st.Device != "" {
			v.Title = name + "に接続しています…"
			v.Subtitle = "Finderにマウントしています"
		} else {
			v.Title = "スマホに接続しています…"
			v.Subtitle = "スマホのロックを解除しておいてください"
		}
		v.Tooltip = "Shioriko — 接続中"
	case service.Mounted, service.Ejecting:
		v.Title = st.Device
		v.Subtitle = st.MountPoint
		v.Tone = "active"
		v.MountPoint = st.MountPoint
		v.CanReveal = st.MountPoint != ""
		v.EjectTitle = name + "を取り出す"
		v.CanEject = st.Phase == service.Mounted
		v.Tooltip = "Shioriko — " + st.Device + " をマウント中"
		if st.Phase == service.Ejecting {
			v.Title = name + "を取り出しています…"
			v.Tooltip = "Shioriko — 取り出し中"
		}
		for _, s := range st.Storages {
			v.Storages = append(v.Storages, makeStorage(s))
		}
		if st.StoragesKnown && len(st.Storages) == 0 {
			v.Notes = append(v.Notes, "ファイルを表示するには、スマホのロックを解除してデータへのアクセスを許可してください")
		}
		if a := activity(st.ReadRate, st.WriteRate); a != "" {
			v.Notes = append(v.Notes, a)
			v.Active = true
		}
	case service.Ejected:
		v.Title = name + "を取り出しました"
		v.Subtitle = "ケーブルを挿し直すと、もう一度マウントします"
		v.CanMount, v.MountTitle = true, name+"をマウント"
		v.Tooltip = "Shioriko — 取り出し済み"
	case service.Failed:
		v.Title = "マウントできませんでした"
		v.Subtitle = describeError(st.Err)
		v.Tone = "warning"
		v.CanMount, v.MountTitle = true, "今すぐ再試行"
		v.Tooltip = "Shioriko — エラー"
	case service.Stopped:
		v.Title = "停止しました"
		v.CanPause = false
	}
	return v
}

// startingView is shown before the service runs.
func startingView() view {
	return view{Phase: "starting", Tone: "idle", Title: "起動しています…", Tooltip: "Shioriko"}
}

// quittingView is shown while the app unmounts before quitting.
func quittingView() view {
	return view{Phase: "quitting", Tone: "idle", Title: "終了しています…", Subtitle: "ボリュームを取り外しています", Tooltip: "Shioriko — 終了中"}
}

func makeStorage(s mtpdev.Storage) storageView {
	sv := storageView{Name: s.Description}
	if s.Capacity > 0 {
		free := min(s.Free, s.Capacity)
		sv.Fraction = float64(s.Capacity-free) / float64(s.Capacity)
		sv.Warn = sv.Fraction >= 0.9
		sv.Detail = fmt.Sprintf("空き %s / %s", formatBytes(float64(free)), formatBytes(float64(s.Capacity)))
	} else {
		sv.Detail = "容量不明"
	}
	if s.ReadOnly {
		sv.Detail += "（読み取り専用）"
	}
	return sv
}

// activityFloor hides transfer rates too small to be a real file transfer
// (directory listings and pings).
const activityFloor = 32 * 1000

func activity(read, write float64) string {
	var parts []string
	if read >= activityFloor {
		parts = append(parts, "↓ 読み込み "+formatBytes(read)+"/s")
	}
	if write >= activityFloor {
		parts = append(parts, "↑ 書き込み "+formatBytes(write)+"/s")
	}
	if len(parts) == 0 {
		return ""
	}
	return "転送中  " + strings.Join(parts, "  ")
}

// formatBytes formats a size with decimal units, like Finder.
func formatBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1000 && i < len(units)-1 {
		n /= 1000
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d %s", int(n), units[i])
	case n < 10:
		// One decimal for small values, without a trailing ".0".
		r := math.Round(n*10) / 10
		if r == math.Trunc(r) {
			return fmt.Sprintf("%d %s", int(r), units[i])
		}
		return fmt.Sprintf("%.1f %s", r, units[i])
	default:
		return fmt.Sprintf("%d %s", int(math.Round(n)), units[i])
	}
}

// describeError explains a connect or mount failure in plain words.
func describeError(err error) string {
	if err == nil {
		return "自動的に再試行します"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "more than 1 device"):
		return "端末が複数あります。1台だけ接続してください"
	case errors.Is(err, mtpdev.ErrDeviceLost):
		return "通信が途切れました。ケーブルを確認してください"
	case strings.Contains(msg, "device not ready"):
		return "スマホが応答しません。ロックを解除してください"
	case strings.Contains(msg, "open session"), strings.Contains(msg, "GetDeviceInfo"):
		return "通信を開始できません。挿し直してください"
	case strings.Contains(msg, "osascript"), strings.Contains(msg, "did not appear"), strings.Contains(msg, "context deadline"):
		return "macOSがボリュームを作成できませんでした"
	}
	return "自動的に再試行します（詳細はログを参照）"
}
