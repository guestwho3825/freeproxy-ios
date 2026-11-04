package core

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// session holds per-player feature state. All features are client-side
// tricks (packets sent to your own game), so they only affect you.
type session struct {
	client, server *minecraft.Conn
	rid            uint64
	uid            int64

	mu        sync.Mutex
	abilities *protocol.AbilityData
	movement  *protocol.Attribute

	fly       bool
	flySpeed  float32
	speed     float32
	jetpack   bool
	jetPower  float32
	nv        bool
	antiKB    bool
	noWeather bool
	coords    bool
	timeLock  int32

	yaw, pitch float32
	pos        mgl32.Vec3
	jumpHeld   bool

	players map[uuid.UUID]string
}

func newSession(client, server *minecraft.Conn) *session {
	gd := server.GameData()
	return &session{
		client: client, server: server,
		rid: gd.EntityRuntimeID, uid: gd.EntityUniqueID,
		flySpeed: 1, speed: 1, jetPower: 1, timeLock: -1,
		yaw: gd.Yaw, pos: gd.PlayerPosition,
		players: map[uuid.UUID]string{},
	}
}

func (s *session) chat(msg string) {
	_ = s.client.WritePacket(&packet.Text{TextType: packet.TextTypeRaw, Message: "§7[§bFreeProxy§7]§r " + msg})
}

// ---------- packets from your phone ----------

func (s *session) fromClient(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.Text:
		if strings.HasPrefix(strings.TrimSpace(p.Message), ".") {
			s.command(strings.TrimSpace(p.Message)[1:])
			return false // never send commands to the server
		}
	case *packet.PlayerAuthInput:
		s.mu.Lock()
		s.yaw, s.pitch, s.pos = p.Yaw, p.Pitch, p.Position
		s.jumpHeld = p.InputData.Load(packet.InputFlagJumpDown) || p.InputData.Load(packet.InputFlagJumping)
		s.mu.Unlock()
	}
	return true
}

// ---------- packets from the server ----------

func (s *session) fromServer(pk packet.Packet) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch p := pk.(type) {
	case *packet.UpdateAbilities:
		if p.AbilityData.EntityUniqueID == s.uid {
			d := p.AbilityData
			s.abilities = &d
			if s.fly || s.flySpeed != 1 {
				s.applyAbilities(&p.AbilityData)
			}
		}
	case *packet.UpdateAttributes:
		if p.EntityRuntimeID == s.rid {
			for i := range p.Attributes {
				if p.Attributes[i].Name == "minecraft:movement" {
					a := p.Attributes[i]
					s.movement = &a
					p.Attributes[i].Value *= s.speed
				}
			}
		}
	case *packet.SetActorMotion:
		if s.antiKB && p.EntityRuntimeID == s.rid {
			return false
		}
	case *packet.SetTime:
		if s.timeLock >= 0 {
			return false
		}
	case *packet.LevelEvent:
		if s.noWeather && (p.EventType == packet.LevelEventStartRaining || p.EventType == packet.LevelEventStartThunderstorm) {
			return false
		}
	case *packet.MobEffect:
		if s.nv && p.EntityRuntimeID == s.rid && p.EffectType == packet.EffectNightVision && p.Operation == packet.MobEffectRemove {
			return false
		}
	case *packet.GameRulesChanged:
		for i, r := range p.GameRules {
			if s.coords && r.Name == "showcoordinates" {
				p.GameRules[i].Value = true
			}
			if s.timeLock >= 0 && r.Name == "dodaylightcycle" {
				p.GameRules[i].Value = false
			}
		}
	case *packet.PlayerList:
		for _, e := range p.Entries {
			if e.ActionType == protocol.PlayerListActionRemove {
				delete(s.players, e.UUID)
			} else if e.Username != "" {
				s.players[e.UUID] = e.Username
			}
		}
	}
	return true
}

// ---------- helpers that send fake packets to your game ----------

func (s *session) applyAbilities(d *protocol.AbilityData) {
	found := false
	for i := range d.Layers {
		if d.Layers[i].Type != protocol.AbilityLayerTypeBase {
			continue
		}
		found = true
		l := &d.Layers[i]
		l.Abilities |= protocol.AbilityMayFly | protocol.AbilityFlying | protocol.AbilityFlySpeed | protocol.AbilityVerticalFlySpeed
		if s.fly {
			l.Values |= protocol.AbilityMayFly
		} else {
			l.Values &^= protocol.AbilityMayFly | protocol.AbilityFlying
		}
		l.FlySpeed = protocol.AbilityBaseFlySpeed * s.flySpeed
		l.VerticalFlySpeed = protocol.AbilityBaseVerticalFlySpeed * s.flySpeed
	}
	if !found {
		vals := uint32(protocol.AbilityBuild | protocol.AbilityMine | protocol.AbilityDoorsAndSwitches |
			protocol.AbilityOpenContainers | protocol.AbilityAttackPlayers | protocol.AbilityAttackMobs)
		if s.fly {
			vals |= protocol.AbilityMayFly
		}
		d.Layers = append(d.Layers, protocol.AbilityLayer{
			Type:             protocol.AbilityLayerTypeBase,
			Abilities:        protocol.AbilityCount - 1,
			Values:           vals,
			FlySpeed:         protocol.AbilityBaseFlySpeed * s.flySpeed,
			VerticalFlySpeed: protocol.AbilityBaseVerticalFlySpeed * s.flySpeed,
			WalkSpeed:        protocol.AbilityBaseWalkSpeed,
		})
	}
}

func (s *session) sendAbilities() {
	var d protocol.AbilityData
	if s.abilities != nil {
		d = *s.abilities
		d.Layers = append([]protocol.AbilityLayer(nil), s.abilities.Layers...)
	} else {
		d = protocol.AbilityData{EntityUniqueID: s.uid, PlayerPermissions: s.server.GameData().PlayerPermissions}
	}
	s.applyAbilities(&d)
	_ = s.client.WritePacket(&packet.UpdateAbilities{AbilityData: d})
}

func (s *session) sendSpeed() {
	a := protocol.Attribute{
		AttributeValue: protocol.AttributeValue{Name: "minecraft:movement", Value: 0.1, Min: 0, Max: math.MaxFloat32},
		DefaultMin:     0, DefaultMax: math.MaxFloat32, Default: 0.1,
	}
	if s.movement != nil {
		a = *s.movement
	}
	a.Value *= s.speed
	_ = s.client.WritePacket(&packet.UpdateAttributes{EntityRuntimeID: s.rid, Attributes: []protocol.Attribute{a}})
}

func (s *session) sendNightVision(on bool) {
	op := byte(packet.MobEffectAdd)
	if !on {
		op = packet.MobEffectRemove
	}
	_ = s.client.WritePacket(&packet.MobEffect{EntityRuntimeID: s.rid, Operation: op, EffectType: packet.EffectNightVision, Duration: 20 * 60 * 60, Amplifier: 0})
}

func (s *session) gamerule(name string, v any) {
	_ = s.client.WritePacket(&packet.GameRulesChanged{GameRules: []protocol.GameRule{{Name: name, Value: v}}})
}

func (s *session) lookDir() mgl32.Vec3 {
	yaw := float64(s.yaw) * math.Pi / 180
	pitch := float64(s.pitch) * math.Pi / 180
	return mgl32.Vec3{
		float32(-math.Sin(yaw) * math.Cos(pitch)),
		float32(-math.Sin(pitch)),
		float32(math.Cos(yaw) * math.Cos(pitch)),
	}
}

func (s *session) push(v mgl32.Vec3) {
	_ = s.client.WritePacket(&packet.SetActorMotion{EntityRuntimeID: s.rid, Velocity: v})
}

// tickLoop runs the jetpack and keeps night vision up.
func (s *session) tickLoop(done chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		n++
		s.mu.Lock()
		if s.jetpack && s.jumpHeld {
			d := s.lookDir().Mul(0.8 * s.jetPower)
			d[1] += 0.35 * s.jetPower
			s.push(d)
		}
		if s.nv && n%300 == 0 { // refresh every 30s
			s.sendNightVision(true)
		}
		s.mu.Unlock()
	}
}

// ---------- chat commands ----------

func onOff(b bool) string {
	if b {
		return "§aON"
	}
	return "§cOFF"
}

func parseNum(args []string, def, min, max float32) float32 {
	if len(args) == 0 {
		return def
	}
	f, err := strconv.ParseFloat(args[0], 32)
	if err != nil {
		return def
	}
	return float32(math.Max(float64(min), math.Min(float64(max), f)))
}

func (s *session) command(line string) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return
	}
	cmd, args := strings.ToLower(parts[0]), parts[1:]
	s.mu.Lock()
	defer s.mu.Unlock()

	switch cmd {
	case "help", "h", "?":
		s.chat("§eCommands:")
		for _, l := range []string{
			".fly §7- toggle flying (double-tap jump)",
			".flyspeed <0.5-10> §7- how fast you fly",
			".speed <0.5-5> §7- walk speed multiplier (1 = normal)",
			".jetpack [power] §7- hold jump to rocket where you look",
			".boost [power] §7- one big launch where you look",
			".up [blocks] §7- pop straight up",
			".nv §7- night vision / fullbright",
			".antikb §7- no knockback",
			".day / .night / .time <0-24000> / .time off",
			".noweather §7- clear rain/thunder (only for you)",
			".coords §7- show coordinates",
			".pos §7- print your exact position",
			".players §7- list players online",
			".status §7- what's turned on",
			".off §7- turn everything off",
		} {
			s.chat("§b" + l)
		}
		s.chat("§7Tip: if you get snapped back, the Realm is blocking that move. Try lower speeds.")

	case "fly", "f":
		s.fly = !s.fly
		s.sendAbilities()
		s.chat("Fly " + onOff(s.fly) + "§r" + map[bool]string{true: " §7(double-tap jump)", false: ""}[s.fly])

	case "flyspeed", "fs":
		s.flySpeed = parseNum(args, 2, 0.5, 10)
		s.sendAbilities()
		s.chat(fmt.Sprintf("Fly speed §e%.1fx", s.flySpeed))

	case "speed", "s":
		if len(args) == 0 && s.speed != 1 {
			s.speed = 1
		} else {
			s.speed = parseNum(args, 1.5, 0.5, 5)
		}
		s.sendSpeed()
		s.chat(fmt.Sprintf("Speed §e%.1fx", s.speed))

	case "jetpack", "jp":
		if len(args) > 0 {
			s.jetPower = parseNum(args, 1, 0.2, 5)
			s.jetpack = true
		} else {
			s.jetpack = !s.jetpack
		}
		s.chat("Jetpack " + onOff(s.jetpack) + fmt.Sprintf(" §7(power %.1f, hold jump)", s.jetPower))

	case "boost", "b":
		p := parseNum(args, 2, 0.2, 10)
		d := s.lookDir().Mul(p)
		d[1] += 0.3
		s.push(d)

	case "up":
		blocks := parseNum(args, 5, 1, 50)
		s.push(mgl32.Vec3{0, float32(math.Sqrt(float64(blocks) * 0.16)), 0})

	case "nv", "fullbright":
		s.nv = !s.nv
		s.sendNightVision(s.nv)
		s.chat("Night vision " + onOff(s.nv))

	case "antikb", "velocity", "kb":
		s.antiKB = !s.antiKB
		s.chat("Anti-knockback " + onOff(s.antiKB))

	case "day":
		s.timeLock = 1000
		s.gamerule("dodaylightcycle", false)
		_ = s.client.WritePacket(&packet.SetTime{Time: s.timeLock})
		s.chat("Time locked to §eday§r (only for you)")
	case "night":
		s.timeLock = 18000
		s.gamerule("dodaylightcycle", false)
		_ = s.client.WritePacket(&packet.SetTime{Time: s.timeLock})
		s.chat("Time locked to §enight§r (only for you)")
	case "time":
		if len(args) > 0 && args[0] == "off" {
			s.timeLock = -1
			s.gamerule("dodaylightcycle", true)
			s.chat("Time back to normal (fixes itself within a few seconds)")
			return
		}
		s.timeLock = int32(parseNum(args, 6000, 0, 24000))
		s.gamerule("dodaylightcycle", false)
		_ = s.client.WritePacket(&packet.SetTime{Time: s.timeLock})
		s.chat(fmt.Sprintf("Time locked to §e%d", s.timeLock))

	case "noweather", "weather":
		s.noWeather = !s.noWeather
		if s.noWeather {
			_ = s.client.WritePacket(&packet.LevelEvent{EventType: packet.LevelEventStopRaining})
			_ = s.client.WritePacket(&packet.LevelEvent{EventType: packet.LevelEventStopThunderstorm})
		}
		s.chat("No weather " + onOff(s.noWeather))

	case "coords", "c":
		s.coords = !s.coords
		s.gamerule("showcoordinates", s.coords)
		s.chat("Coordinates " + onOff(s.coords))

	case "pos":
		s.chat(fmt.Sprintf("X §e%.1f§r  Y §e%.1f§r  Z §e%.1f", s.pos[0], s.pos[1]-1.62, s.pos[2]))

	case "players", "list":
		names := make([]string, 0, len(s.players))
		for _, n := range s.players {
			names = append(names, n)
		}
		sort.Strings(names)
		s.chat(fmt.Sprintf("§e%d online:§r %s", len(names), strings.Join(names, ", ")))

	case "status":
		s.chat(fmt.Sprintf("Fly %s §r(%.1fx)  Speed §e%.1fx§r  Jetpack %s", onOff(s.fly), s.flySpeed, s.speed, onOff(s.jetpack)))
		s.chat(fmt.Sprintf("NV %s  §rAntiKB %s  §rNoWeather %s  §rCoords %s", onOff(s.nv), onOff(s.antiKB), onOff(s.noWeather), onOff(s.coords)))

	case "off", "panic":
		s.fly, s.flySpeed, s.speed, s.jetpack, s.antiKB, s.noWeather = false, 1, 1, false, false, false
		s.sendAbilities()
		s.sendSpeed()
		if s.nv {
			s.nv = false
			s.sendNightVision(false)
		}
		if s.timeLock >= 0 {
			s.timeLock = -1
			s.gamerule("dodaylightcycle", true)
		}
		s.chat("Everything §cOFF")

	default:
		s.chat("Unknown command §e." + cmd + "§r — type §e.help")
	}
}
