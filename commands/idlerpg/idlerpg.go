package idlerpg

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muesli/ircflu/app"
	"github.com/muesli/ircflu/commands"
	"github.com/muesli/ircflu/msgsystem"
	"github.com/muesli/ircflu/msgsystem/irc"
)

var itemSlots = [10]string{
	"ring", "amulet", "charm", "weapon", "helm",
	"tunic", "gloves", "leggings", "shield", "boots",
}

type Player struct {
	Nick     string
	Class    string
	PassHash string
	Level    int
	TTL      int64  // seconds until next level
	Items    [10]int // item level per slot
	Online   bool
	Addr     string // nick!user@host when online
}

func (p *Player) itemSum() int {
	s := 0
	for _, v := range p.Items {
		s += v
	}
	return s
}

type IdleRPGCommand struct {
	messagesIn  chan msgsystem.Message
	messagesOut chan msgsystem.Message

	players  map[string]*Player // keyed by lowercase nick
	mu       sync.Mutex

	dataFile string
	channel  string
}

func (cmd *IdleRPGCommand) Name() string {
	return "idlerpg"
}

func (cmd *IdleRPGCommand) Run(channelIn, channelOut chan msgsystem.Message) {
	cmd.messagesIn = channelIn
	cmd.messagesOut = channelOut
	cmd.players = make(map[string]*Player)
	cmd.load()

	go cmd.tick()

	if cmd.channel != "" {
		go cmd.joinChannel()
	}
}

func (cmd *IdleRPGCommand) joinChannel() {
	for {
		time.Sleep(2 * time.Second)
		sub := msgsystem.GetSubSystem("irc")
		if sub == nil {
			continue
		}
		ircclient, ok := (*sub).(*irc.IrcSubSystem)
		if !ok || ircclient == nil {
			continue
		}
		ircclient.Join(cmd.channel)
		return
	}
}

func (cmd *IdleRPGCommand) Parse(msg msgsystem.Message) bool {
	text := strings.TrimSpace(msg.Msg)
	nick := extractNick(msg.Source)

	// IRC system events injected with null-byte prefix
	if strings.HasPrefix(text, "\x00") {
		return cmd.handleEvent(text[1:], nick, msg.Source)
	}

	channel := ""
	if len(msg.To) > 0 {
		channel = msg.To[0]
	}

	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}

	switch fields[0] {
	case "!register":
		if len(fields) < 4 {
			cmd.reply(msg, "Usage: !register <nick> <class> <password>")
			return true
		}
		return cmd.cmdRegister(msg, channel, fields[1], strings.Join(fields[2:len(fields)-1], " "), fields[len(fields)-1])

	case "!login":
		if len(fields) < 2 {
			cmd.reply(msg, "Usage: !login <password>")
			return true
		}
		return cmd.cmdLogin(msg, channel, nick, fields[1])

	case "!logout":
		return cmd.cmdLogout(msg, channel, nick)

	case "!status":
		target := nick
		if len(fields) >= 2 {
			target = strings.ToLower(fields[1])
		}
		return cmd.cmdStatus(msg, channel, target)

	case "!whoami":
		return cmd.cmdStatus(msg, channel, strings.ToLower(nick))

	case "!top":
		return cmd.cmdTop(msg, channel)

	case "!help":
		cmd.reply(msg, "IdleRPG commands: "+
			"!register <nick> <class> <pass> — create character | "+
			"!login <pass> — log in (stay idle to level up!) | "+
			"!logout — go offline | "+
			"!status [nick] — show level, TTL, item total | "+
			"!whoami — your own status | "+
			"!top — top 5 players. "+
			"Talking, changing nick, parting or quitting adds penalty time.")
		return true

	default:
		// Penalize online player for talking
		if !strings.HasPrefix(text, "!") {
			cmd.mu.Lock()
			p := cmd.findByAddr(msg.Source)
			cmd.mu.Unlock()
			if p != nil {
				cmd.mu.Lock()
				cmd.applyPenalty(p, int64(len(text)))
				cmd.mu.Unlock()
				cmd.save()
			}
		}
		return false
	}
}

func (cmd *IdleRPGCommand) handleEvent(event, nick, addr string) bool {
	parts := strings.SplitN(event, " ", 2)
	evType := parts[0]

	cmd.mu.Lock()
	p := cmd.findByAddr(addr)
	if p == nil {
		// also try by nick for QUIT (addr might differ)
		p = cmd.players[strings.ToLower(nick)]
		if p != nil && !p.Online {
			p = nil
		}
	}
	cmd.mu.Unlock()

	if p == nil {
		return false
	}

	var base int64
	switch evType {
	case "QUIT":
		base = 20
	case "PART":
		base = 200
		cmd.mu.Lock()
		p.Online = false
		cmd.mu.Unlock()
	case "NICK":
		base = 30
		if len(parts) > 1 {
			newNick := strings.ToLower(parts[1])
			cmd.mu.Lock()
			delete(cmd.players, strings.ToLower(p.Nick))
			p.Nick = parts[1]
			p.Addr = strings.Replace(p.Addr, nick, parts[1], 1)
			cmd.players[newNick] = p
			cmd.mu.Unlock()
		}
	default:
		return false
	}

	cmd.mu.Lock()
	cmd.applyPenalty(p, base)
	cmd.mu.Unlock()
	cmd.save()
	return true
}

func (cmd *IdleRPGCommand) cmdRegister(msg msgsystem.Message, channel, nick, class, pass string) bool {
	key := strings.ToLower(nick)
	cmd.mu.Lock()
	_, exists := cmd.players[key]
	cmd.mu.Unlock()

	if exists {
		cmd.reply(msg, fmt.Sprintf("Nick %s is already registered.", nick))
		return true
	}

	p := &Player{
		Nick:     nick,
		Class:    class,
		PassHash: hashPass(pass),
		Level:    0,
		TTL:      ttlForLevel(0),
	}
	cmd.mu.Lock()
	cmd.players[key] = p
	cmd.mu.Unlock()
	cmd.save()

	cmd.send(channel, fmt.Sprintf("%s, the %s, has registered for IdleRPG! Next level in %s.", nick, class, fmtDuration(p.TTL)))
	return true
}

func (cmd *IdleRPGCommand) cmdLogin(msg msgsystem.Message, channel, nick, pass string) bool {
	key := strings.ToLower(nick)
	cmd.mu.Lock()
	p, ok := cmd.players[key]
	cmd.mu.Unlock()

	if !ok {
		cmd.reply(msg, "No character registered with that nick. Use !register first.")
		return true
	}
	if p.PassHash != hashPass(pass) {
		cmd.reply(msg, "Wrong password.")
		return true
	}
	cmd.mu.Lock()
	p.Online = true
	p.Addr = msg.Source
	cmd.mu.Unlock()
	cmd.save()

	cmd.send(channel, fmt.Sprintf("%s, the level %d %s, has logged in! Next level in %s.", nick, p.Level, p.Class, fmtDuration(p.TTL)))
	return true
}

func (cmd *IdleRPGCommand) cmdLogout(msg msgsystem.Message, channel, nick string) bool {
	cmd.mu.Lock()
	p := cmd.findByAddr(msg.Source)
	if p != nil {
		p.Online = false
	}
	cmd.mu.Unlock()
	if p != nil {
		cmd.save()
		cmd.send(channel, fmt.Sprintf("%s has logged out of IdleRPG.", nick))
	}
	return true
}

func (cmd *IdleRPGCommand) cmdStatus(msg msgsystem.Message, channel, nick string) bool {
	cmd.mu.Lock()
	p, ok := cmd.players[strings.ToLower(nick)]
	cmd.mu.Unlock()

	if !ok {
		cmd.reply(msg, fmt.Sprintf("No character found for %s.", nick))
		return true
	}

	status := "offline"
	if p.Online {
		status = "online"
	}
	cmd.reply(msg, fmt.Sprintf("%s, the level %d %s [%s] — TTL: %s — Items: %d",
		p.Nick, p.Level, p.Class, status, fmtDuration(p.TTL), p.itemSum()))
	return true
}

func (cmd *IdleRPGCommand) cmdTop(msg msgsystem.Message, channel string) bool {
	cmd.mu.Lock()
	players := make([]*Player, 0, len(cmd.players))
	for _, p := range cmd.players {
		players = append(players, p)
	}
	cmd.mu.Unlock()

	sort.Slice(players, func(i, j int) bool {
		if players[i].Level != players[j].Level {
			return players[i].Level > players[j].Level
		}
		return players[i].TTL < players[j].TTL
	})

	n := 5
	if len(players) < n {
		n = len(players)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		p := players[i]
		parts[i] = fmt.Sprintf("%d. %s (lvl %d)", i+1, p.Nick, p.Level)
	}
	cmd.reply(msg, "Top players: "+strings.Join(parts, " | "))
	return true
}

// tick runs every second, driving the game clock.
func (cmd *IdleRPGCommand) tick() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cmd.mu.Lock()
		var levelUps []*Player
		for _, p := range cmd.players {
			if !p.Online {
				continue
			}
			p.TTL--
			if p.TTL <= 0 {
				levelUps = append(levelUps, p)
			} else {
				// random events: ~1/86400 chance per tick per player
				if rand.Intn(86400) == 0 {
					cmd.randomEvent(p)
				}
			}
		}
		cmd.mu.Unlock()

		for _, p := range levelUps {
			cmd.doLevelUp(p)
		}
		if len(levelUps) > 0 {
			cmd.save()
		}
	}
}

func (cmd *IdleRPGCommand) doLevelUp(p *Player) {
	cmd.mu.Lock()
	p.Level++
	p.TTL = ttlForLevel(p.Level)

	// Find an item
	slot := rand.Intn(10)
	maxItem := int(math.Max(float64(int(float64(p.Level)*1.5)), 1))
	itemLevel := rand.Intn(maxItem) + 1
	improved := itemLevel > p.Items[slot]
	if improved {
		p.Items[slot] = itemLevel
	}
	slotName := itemSlots[slot]
	nick := p.Nick
	level := p.Level
	ttl := p.TTL
	isum := p.itemSum()
	cmd.mu.Unlock()

	msg := fmt.Sprintf("%s has attained level %d! Next level in %s. They find a %s of level %d",
		nick, level, fmtDuration(ttl), slotName, itemLevel)
	if improved {
		msg += " (equipped!)"
	}
	msg += fmt.Sprintf(" [item total: %d]", isum)
	cmd.send(cmd.channel, msg)

	// Battle: always challenge a random online player
	cmd.mu.Lock()
	var opponents []*Player
	for _, op := range cmd.players {
		if op.Online && strings.ToLower(op.Nick) != strings.ToLower(nick) {
			opponents = append(opponents, op)
		}
	}
	cmd.mu.Unlock()

	if len(opponents) > 0 {
		opponent := opponents[rand.Intn(len(opponents))]
		cmd.battle(p, opponent)
	}
}

func (cmd *IdleRPGCommand) battle(a, b *Player) {
	cmd.mu.Lock()

	aSum := a.itemSum()
	bSum := b.itemSum()
	if aSum < 1 {
		aSum = 1
	}
	if bSum < 1 {
		bSum = 1
	}

	aRoll := rand.Intn(aSum)
	bRoll := rand.Intn(bSum)

	var winner, loser *Player
	if aRoll >= bRoll {
		winner, loser = a, b
	} else {
		winner, loser = b, a
	}

	pct := int(math.Max(float64(loser.Level)/4.0, 7))
	change := winner.TTL * int64(pct) / 100
	if change < 1 {
		change = 1
	}
	winner.TTL -= change
	if winner.TTL < 0 {
		winner.TTL = 0
	}
	loser.TTL += change

	wName, wRoll := winner.Nick, aRoll
	lName, lRoll := loser.Nick, bRoll
	if winner == b {
		wRoll, lRoll = bRoll, aRoll
	}
	cmd.mu.Unlock()

	cmd.send(cmd.channel, fmt.Sprintf(
		"%s [%d/%d] battles %s [%d/%d] and wins! TTL adjusted by %d%%.",
		wName, wRoll, aSum, lName, lRoll, bSum, pct))
}

func (cmd *IdleRPGCommand) randomEvent(p *Player) {
	pct := rand.Intn(8) + 5 // 5-12%
	change := p.TTL * int64(pct) / 100
	if change < 1 {
		change = 1
	}
	if rand.Intn(2) == 0 {
		// Calamity
		p.TTL += change
		go cmd.send(cmd.channel, fmt.Sprintf(
			"%s has been struck by misfortune! TTL increased by %d%%.", p.Nick, pct))
	} else {
		// Godsend
		p.TTL -= change
		if p.TTL < 1 {
			p.TTL = 1
		}
		go cmd.send(cmd.channel, fmt.Sprintf(
			"The gods smile upon %s! TTL reduced by %d%%.", p.Nick, pct))
	}
}

// applyPenalty adds base * 1.14^level seconds. Must be called with mu held.
func (cmd *IdleRPGCommand) applyPenalty(p *Player, base int64) {
	penalty := int64(float64(base) * math.Pow(1.14, float64(p.Level)))
	p.TTL += penalty
}

func (cmd *IdleRPGCommand) findByAddr(addr string) *Player {
	lAddr := strings.ToLower(addr)
	for _, p := range cmd.players {
		if p.Online && strings.ToLower(p.Addr) == lAddr {
			return p
		}
	}
	return nil
}

func (cmd *IdleRPGCommand) send(to, msg string) {
	m := msgsystem.Message{Msg: msg}
	if to != "" {
		m.To = []string{to}
	}
	cmd.messagesOut <- m
}

func (cmd *IdleRPGCommand) reply(msg msgsystem.Message, text string) {
	m := msgsystem.Message{Msg: text}
	if len(msg.To) > 0 {
		m.To = msg.To
	}
	cmd.messagesOut <- m
}

func (cmd *IdleRPGCommand) save() {
	if cmd.dataFile == "" {
		return
	}
	cmd.mu.Lock()
	data, err := json.MarshalIndent(cmd.players, "", "  ")
	cmd.mu.Unlock()
	if err != nil {
		fmt.Println("IdleRPG: save error:", err)
		return
	}
	if err := os.WriteFile(cmd.dataFile, data, 0644); err != nil {
		fmt.Println("IdleRPG: write error:", err)
	}
}

func (cmd *IdleRPGCommand) load() {
	if cmd.dataFile == "" {
		return
	}
	data, err := os.ReadFile(cmd.dataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Println("IdleRPG: load error:", err)
		}
		return
	}
	if err := json.Unmarshal(data, &cmd.players); err != nil {
		fmt.Println("IdleRPG: parse error:", err)
	}
	// All players start offline after bot restart
	for _, p := range cmd.players {
		p.Online = false
		p.Addr = ""
	}
	fmt.Printf("IdleRPG: loaded %d players\n", len(cmd.players))
}

func ttlForLevel(level int) int64 {
	return int64(600 * math.Pow(1.16, float64(level)))
}

func hashPass(pass string) string {
	h := sha256.Sum256([]byte(pass))
	return fmt.Sprintf("%x", h)
}

func extractNick(src string) string {
	if idx := strings.Index(src, "!"); idx > 0 {
		return src[:idx]
	}
	return src
}

func fmtDuration(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func init() {
	cmd := IdleRPGCommand{}

	app.AddFlags([]app.CliFlag{
		{V: &cmd.dataFile, Name: "idlerpgfile", Value: "idlerpg.json", Desc: "Path to IdleRPG player data file"},
		{V: &cmd.channel, Name: "idlerpgchannel", Value: "", Desc: "IRC channel for IdleRPG game announcements"},
	})

	commands.RegisterCommand(&cmd)
}
