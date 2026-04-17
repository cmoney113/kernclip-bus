package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ── Command Dispatcher ────────────────────────────────────────────────────────
//
// Subscribes to "action" topics and executes side effects on the host system.
// Every command topic publishes a result ACK back to "gtt.system.ack.<topic>".
//
// Supported topic → action mapping:
//
//   gtt.settings.brightness   data="<0-100>"   → sets screen backlight
//   gtt.settings.volume       data="<0-100>"   → sets PipeWire/PulseAudio volume
//   gtt.settings.nightlight   data="true|false" → toggles GNOME Night Light
//   gtt.settings.dconf        data="<path> <type> <value>" → dconf write
//   gtt.settings.gsettings    data="<schema> <key> <value>" → gsettings set
//   gtt.system.kill           data="<pid>"     → kill -9 <pid>
//   gtt.system.killname       data="<name>"    → pkill -9 <name>
//   gtt.system.service        data="<action>:<service>" → systemctl <action> <service>
//   gtt.system.notify         data="<title>|<body>" → desktop notification
//   gtt.system.exec           data="<shell cmd>" → executes shell command (auth required)

// commandTopics lists all topics the dispatcher subscribes to.
var commandTopics = []string{
	"gtt.settings.brightness",
	"gtt.settings.volume",
	"gtt.settings.nightlight",
	"gtt.settings.dconf",
	"gtt.settings.gsettings",
	"gtt.system.kill",
	"gtt.system.killname",
	"gtt.system.service",
	"gtt.system.notify",
	"gtt.system.exec",
}

// ackTopic returns the ACK topic name for a command topic.
func ackTopic(cmdTopic string) string {
	return "gtt.system.ack." + strings.ReplaceAll(strings.TrimPrefix(cmdTopic, "gtt."), ".", "_")
}

// publishACK sends a result message back on the ack topic.
func publishACK(bus *Bus, cmdTopic string, ok bool, detail string) {
	topic := ackTopic(cmdTopic)
	payload := map[string]interface{}{
		"ok":        ok,
		"command":   cmdTopic,
		"detail":    detail,
		"timestamp": float64(time.Now().UnixMilli()) / 1000.0,
	}
	data, _ := json.Marshal(payload)
	t := bus.getOrCreate(topic)
	msg := Message{
		Topic:  topic,
		Mime:   "application/json",
		Data:   string(data),
		Sender: "kernclip-busd/cmddispatch",
	}
	published := t.publish(msg)
	bus.patterns.FanOut(topic, published)
}

// ── Individual command handlers ───────────────────────────────────────────────

func handleBrightness(data string) (string, error) {
	pct, err := strconv.Atoi(strings.TrimSpace(data))
	if err != nil || pct < 0 || pct > 100 {
		return "", fmt.Errorf("brightness must be 0-100, got %q", data)
	}
	// Try brightnessctl first (most common on modern Linux desktops)
	if out, err := exec.Command("brightnessctl", "set", fmt.Sprintf("%d%%", pct)).CombinedOutput(); err == nil {
		return string(out), nil
	}
	// Fallback: xrandr (X11)
	if out, err := exec.Command("xrandr", "--output", "eDP-1", "--brightness",
		fmt.Sprintf("%.2f", float64(pct)/100.0)).CombinedOutput(); err == nil {
		return string(out), nil
	}
	return "", fmt.Errorf("no brightness control available (tried brightnessctl, xrandr)")
}

func handleVolume(data string) (string, error) {
	pct, err := strconv.Atoi(strings.TrimSpace(data))
	if err != nil || pct < 0 || pct > 150 {
		return "", fmt.Errorf("volume must be 0-150, got %q", data)
	}
	// Try wpctl (PipeWire) first
	if out, err := exec.Command("wpctl", "set-volume", "@DEFAULT_AUDIO_SINK@",
		fmt.Sprintf("%d%%", pct)).CombinedOutput(); err == nil {
		return string(out), nil
	}
	// Fallback: pactl (PulseAudio)
	if out, err := exec.Command("pactl", "set-sink-volume", "@DEFAULT_SINK@",
		fmt.Sprintf("%d%%", pct)).CombinedOutput(); err == nil {
		return string(out), nil
	}
	// Fallback: amixer
	out, err := exec.Command("amixer", "-q", "sset", "Master", fmt.Sprintf("%d%%", pct)).CombinedOutput()
	return string(out), err
}

func handleNightLight(data string) (string, error) {
	val := strings.TrimSpace(strings.ToLower(data))
	enabled := val == "true" || val == "1" || val == "on" || val == "yes"
	boolStr := "false"
	if enabled {
		boolStr = "true"
	}
	out, err := exec.Command("gsettings", "set",
		"org.gnome.settings-daemon.plugins.color",
		"night-light-enabled", boolStr).CombinedOutput()
	return string(out), err
}

func handleDConf(data string) (string, error) {
	// data: "/path/to/key value" (dconf write format)
	// e.g. "/org/gnome/desktop/background/picture-uri 'file:///home/...'"
	parts := strings.SplitN(strings.TrimSpace(data), " ", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("dconf format: '<dconf-path> <value>', got %q", data)
	}
	out, err := exec.Command("dconf", "write", parts[0], parts[1]).CombinedOutput()
	return string(out), err
}

func handleGSettings(data string) (string, error) {
	// data: "schema key value"
	// e.g. "org.gnome.desktop.interface icon-theme 'Papirus'"
	parts := strings.SplitN(strings.TrimSpace(data), " ", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("gsettings format: '<schema> <key> <value>', got %q", data)
	}
	out, err := exec.Command("gsettings", "set", parts[0], parts[1], parts[2]).CombinedOutput()
	return string(out), err
}

func handleKill(data string) (string, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(data))
	if err != nil {
		return "", fmt.Errorf("kill requires numeric pid, got %q", data)
	}
	out, err := exec.Command("kill", "-9", strconv.Itoa(pid)).CombinedOutput()
	return string(out), err
}

func handleKillName(data string) (string, error) {
	name := strings.TrimSpace(data)
	if name == "" {
		return "", fmt.Errorf("killname requires process name")
	}
	out, err := exec.Command("pkill", "-9", name).CombinedOutput()
	return string(out), err
}

func handleService(data string) (string, error) {
	// data: "action:service-name"   e.g. "restart:nginx"
	// supported actions: start | stop | restart | enable | disable | mask | unmask
	parts := strings.SplitN(strings.TrimSpace(data), ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("service format: '<action>:<service>', got %q", data)
	}
	action := strings.TrimSpace(parts[0])
	service := strings.TrimSpace(parts[1])
	allowed := map[string]bool{
		"start": true, "stop": true, "restart": true, "reload": true,
		"enable": true, "disable": true, "mask": true, "unmask": true,
		"status": true,
	}
	if !allowed[action] {
		return "", fmt.Errorf("disallowed systemctl action: %q", action)
	}
	out, err := exec.Command("systemctl", action, service).CombinedOutput()
	return string(out), err
}

func handleNotify(data string) (string, error) {
	// data: "Title|Body"  or just "Body"
	parts := strings.SplitN(data, "|", 2)
	title, body := "kc-bus", data
	if len(parts) == 2 {
		title, body = parts[0], parts[1]
	}
	out, err := exec.Command("notify-send", title, body).CombinedOutput()
	return string(out), err
}

func handleExec(data string) (string, error) {
	cmd := exec.Command("bash", "-c", data)
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,             // Prevent signal propagation
		Pdeathsig: syscall.SIGTERM,  // Die if parent dies
	}
	// Prevent fd leakage to children
	cmd.Env = append(os.Environ(), "KERNCLIP_BUS_FD=CLOSED")

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("exec failed: %w (output: %s)", err, string(out))
	}
	return string(out), nil
}

// ── Dispatcher loop ───────────────────────────────────────────────────────────

func startCommandDispatcher(bus *Bus) {
	// Fan-in from all command topics via a pattern subscription
	pattern := "gtt.settings.*|gtt.system.kill|gtt.system.killname|gtt.system.service|gtt.system.notify|gtt.system.exec"
	_ = pattern // use individual topic subscriptions instead for clarity

	// Subscribe to each command topic individually, fan into one goroutine
	merged := make(chan Message, 128)

	for _, topicName := range commandTopics {
		tn := topicName // capture
		t := bus.getOrCreate(tn)
		id, ch := t.subscribe()
		go func() {
			defer t.unsubscribe(id)
			for msg := range ch {
				select {
				case merged <- msg:
				default:
					log.Printf("[cmddispatch] dropped slow command on %s", tn)
				}
			}
		}()
	}

	log.Printf("[cmddispatch] command dispatcher ready, listening on %d topics", len(commandTopics))

	go func() {
		for msg := range merged {
			go func(m Message) {
				var (
					out string
					err error
				)
				switch m.Topic {
				case "gtt.settings.brightness":
					out, err = handleBrightness(m.Data)
				case "gtt.settings.volume":
					out, err = handleVolume(m.Data)
				case "gtt.settings.nightlight":
					out, err = handleNightLight(m.Data)
				case "gtt.settings.dconf":
					out, err = handleDConf(m.Data)
				case "gtt.settings.gsettings":
					out, err = handleGSettings(m.Data)
				case "gtt.system.kill":
					out, err = handleKill(m.Data)
				case "gtt.system.killname":
					out, err = handleKillName(m.Data)
				case "gtt.system.service":
					out, err = handleService(m.Data)
				case "gtt.system.notify":
					out, err = handleNotify(m.Data)
				case "gtt.system.exec":
					select {
					case bus.execSem <- struct{}{}:
						defer func() { <-bus.execSem }()
						out, err = handleExec(m.Data)
					default:
						log.Printf("[cmddispatch] exec rejected: too many concurrent commands")
						publishACK(bus, m.Topic, false, "rate limited: system busy")
						return
					}
				default:
					log.Printf("[cmddispatch] unhandled topic: %s", m.Topic)
					return
				}

				if err != nil {
					log.Printf("[cmddispatch] %s error: %v", m.Topic, err)
					publishACK(bus, m.Topic, false, err.Error())
				} else {
					detail := strings.TrimSpace(out)
					if detail == "" {
						detail = "ok"
					}
					log.Printf("[cmddispatch] %s → %s", m.Topic, detail)
					publishACK(bus, m.Topic, true, detail)
				}
			}(msg)
		}
	}()
}
