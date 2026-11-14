package rolebots

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Who is listening. A line is worth saying only where a person is connected to read it, and
// the chat registry knows that: a member of a channel with a live connection is a person at
// a terminal (a bot member has none, presence.go JoinBot).
// The audience of a corporation's channel is read off m.lsc.Members(corpID), never off who
// is in the corporation: a pilot who changed corporations stays in the old channel until
// they relog (the core sets Member.CorpID once).
// It is polled, not hooked: a poll also sees leaves and reconnects, and needs no hook into
// server/lsc/lscsvc.go.

// corpAudience is one channel's listeners as of the last poll.
type corpAudience struct {
	Humans     map[int64]string // charID → name, live connections only
	HumanTypes map[int64]int64  // charID → bloodline type, for links
	Bots       int              // present bots in the channel
	At         int64
	Alliance   bool // the channel is an alliance's, not a corporation's
}

var (
	audienceMu sync.Mutex
	audiences  = map[int64]*corpAudience{} // by corpID (= channelID)

	// chatterWatch is the set of channels with at least one human, read
	// lock-free by onChat on the sender's goroutine and by chatterNote.
	chatterWatch atomic.Pointer[map[int64]bool]

	// pendingGreet is "corp|human" → due at (unix).
	pendingGreet = map[string]int64{}

	// chatterAllianceChans is every alliance channel a present bot sits in,
	// read lock-free wherever a channel ID has to be told apart: a corp
	// channel and an alliance channel are both a bare owner ID.
	chatterAllianceChans atomic.Pointer[map[int64]bool]
)

// isAllianceChannel reports whether a channel is an alliance's that our bots
// sit in.
func isAllianceChannel(channelID int64) bool {
	p := chatterAllianceChans.Load()
	return p != nil && (*p)[channelID]
}

// chatterWatched reports whether a person is live in this channel.
func chatterWatched(channelID int64) bool {
	p := chatterWatch.Load()
	return p != nil && (*p)[channelID]
}

// chatterHumans snapshots a channel's humans.
func chatterHumans(corpID int64) map[int64]string {
	audienceMu.Lock()
	defer audienceMu.Unlock()
	a := audiences[corpID]
	if a == nil {
		return nil
	}
	out := make(map[int64]string, len(a.Humans))
	for k, v := range a.Humans {
		out[k] = v
	}
	return out
}

// chatterHumanType is a listener's bloodline type, 0 if unknown.
func chatterHumanType(corpID, charID int64) int64 {
	audienceMu.Lock()
	defer audienceMu.Unlock()
	if a := audiences[corpID]; a != nil {
		return a.HumanTypes[charID]
	}
	return 0
}

// audienceChange is one person appearing in or leaving a channel.
type audienceChange struct {
	CorpID  int64
	HumanID int64
	Name    string
	Joined  bool
}

// pollAudience reads every channel a present bot sits in and diffs it
// against the last poll.
func (m *Mod) pollAudience(ctx context.Context) []audienceChange {
	if m.lsc == nil {
		return nil
	}
	// botsByCorp is by channel: a corporation's, and its alliance's, which
	// every member corporation's bots share.
	botsByCorp := map[int64]int{}
	allianceChans := map[int64]bool{}
	presenceMu.Lock()
	for _, r := range presenceApplied {
		if r.CorpID != 0 {
			botsByCorp[r.CorpID]++
		}
		if r.AllianceID != 0 {
			botsByCorp[r.AllianceID]++
			allianceChans[r.AllianceID] = true
		}
	}
	presenceMu.Unlock()
	chatterAllianceChans.Store(&allianceChans)

	now := clock.Unix()
	var changes []audienceChange
	watch := map[int64]bool{}

	audienceMu.Lock()
	defer audienceMu.Unlock()
	seen := map[int64]bool{}
	for corpID, bots := range botsByCorp {
		seen[corpID] = true
		humans := map[int64]string{}
		types := map[int64]int64{}
		for _, mem := range m.lsc.Members(corpID) {
			if mem == nil || mem.Conn == nil || mem.CharID == 0 || m.isSynthetic(mem.CharID) {
				continue
			}
			humans[mem.CharID] = mem.CharName
			types[mem.CharID] = mem.TypeID
		}
		old := audiences[corpID]
		for id, name := range humans {
			if old == nil || old.Humans[id] == "" {
				changes = append(changes, audienceChange{CorpID: corpID, HumanID: id, Name: name, Joined: true})
			}
		}
		if old != nil {
			for id, name := range old.Humans {
				if humans[id] == "" {
					changes = append(changes, audienceChange{CorpID: corpID, HumanID: id, Name: name})
				}
			}
		}
		if len(humans) == 0 {
			delete(audiences, corpID)
			continue
		}
		audiences[corpID] = &corpAudience{Humans: humans, HumanTypes: types, Bots: bots, At: now,
			Alliance: allianceChans[corpID]}
		watch[corpID] = true
	}
	// Channels no bot sits in any more: whoever was there, nobody of ours
	// is listening.
	for corpID, old := range audiences {
		if seen[corpID] {
			continue
		}
		for id, name := range old.Humans {
			changes = append(changes, audienceChange{CorpID: corpID, HumanID: id, Name: name})
		}
		delete(audiences, corpID)
	}
	chatterWatch.Store(&watch)
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].CorpID != changes[j].CorpID {
			return changes[i].CorpID < changes[j].CorpID
		}
		return changes[i].HumanID < changes[j].HumanID
	})
	for _, c := range changes {
		if !c.Joined {
			// A channel that emptied forgets its backlog and its open offer.
			if _, still := audiences[c.CorpID]; !still {
				chatterQ.clear(c.CorpID)
				closeFleetOffer(c.CorpID)
			}
		}
	}
	return changes
}

// audienceRow is one channel for the panel.
type audienceRow struct {
	CorpID   int64 // the channel: a corporation, or an alliance when Alliance
	Alliance bool
	Bots     int
	Humans   []string
	HumanIDs []int64
}

func chatterAudienceRows() []audienceRow {
	audienceMu.Lock()
	defer audienceMu.Unlock()
	var out []audienceRow
	for corpID, a := range audiences {
		row := audienceRow{CorpID: corpID, Alliance: a.Alliance, Bots: a.Bots}
		for id, name := range a.Humans {
			row.Humans = append(row.Humans, name)
			row.HumanIDs = append(row.HumanIDs, id)
		}
		sort.Strings(row.Humans)
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CorpID < out[j].CorpID })
	return out
}

// ── greetings ───────────────────────────────────────────────────────────────

// scheduleGreetings books a hello for each person who just appeared, a few seconds out, once
// per six hours per channel so a reconnect is silent. A farewell, when the switch is on, is
// queued as an ordinary event so it takes the full budget.
func (m *Mod) scheduleGreetings(changes []audienceChange) {
	now := clock.Unix()
	// Corporation channels first, so the alliance's hello can see that a
	// corp greeting is already on its way.
	ordered := make([]audienceChange, 0, len(changes))
	for _, c := range changes {
		if !isAllianceChannel(c.CorpID) {
			ordered = append(ordered, c)
		}
	}
	for _, c := range changes {
		if isAllianceChannel(c.CorpID) {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered {
		if c.Joined {
			if !chatterGreetOn.Load() {
				continue
			}
			if isAllianceChannel(c.CorpID) && (!chatterAllianceGreetOn.Load() || !chatterAllianceOn.Load() ||
				chatterGreetedElsewhere(c.HumanID)) {
				// One hello is enough: a person greeted in their corp channel
				// within the hour is not greeted again by the alliance.
				continue
			}
			if !chatterBook.greetDue(clock.Now(), c.CorpID, c.HumanID, chatterGreetOncePer) {
				continue
			}
			delay := chatterGreetDelayMin + rand.Intn(chatterGreetDelayMax-chatterGreetDelayMin+1)
			audienceMu.Lock()
			pendingGreet[chatterKey(c.CorpID, itoa64(c.HumanID))] = now + int64(delay)
			audienceMu.Unlock()
			continue
		}
		if chatterFarewell.Load() && chatterWatched(c.CorpID) {
			// Only when somebody is left to read it.
			sp, ok := chatterAnyBot(c.CorpID, 0)
			if !ok {
				continue
			}
			m.chatterNote(context.Background(), chatterEvent{
				Kind: "social.ack", ChannelID: c.CorpID, CorpID: c.CorpID, ActorID: sp.CharID,
				Slots: map[string]string{"first": firstName(c.Name)},
			})
		}
	}
}

// fireDueGreetings says the hellos whose time has come, to people still there. A hire line
// queued for the same person (ceo.hr.hire) counts as the greeting.
func (m *Mod) fireDueGreetings(ctx context.Context, now int64) {
	type due struct {
		corpID, humanID int64
	}
	var fire []due
	audienceMu.Lock()
	for k, at := range pendingGreet {
		if at > now {
			continue
		}
		delete(pendingGreet, k)
		corpID, humanID := splitChatterKey(k)
		if a := audiences[corpID]; a != nil && a.Humans[humanID] != "" {
			fire = append(fire, due{corpID, humanID})
		}
	}
	audienceMu.Unlock()
	for _, d := range fire {
		if ctx.Err() != nil {
			return
		}
		if chatterQ.has(d.corpID, "ceo.hr.hire", d.humanID) {
			continue
		}
		humans := chatterHumans(d.corpID)
		name := humans[d.humanID]
		sp, ok := m.chatterGreeter(ctx, d.corpID, nil)
		if !ok {
			continue
		}
		slots := map[string]string{
			"who":   charLink(d.humanID, chatterHumanType(d.corpID, d.humanID), name),
			"first": firstName(name),
			"n":     itoa(len(chatterPresentBots(d.corpID))),
		}
		trait := dominantTrait(m.chatterTraitsOf(ctx, sp.CharID))
		tmplID, text, ok := renderChatter("social.greet", trait, slots, rand.Int())
		if !ok {
			chatterDrop("render")
			continue
		}
		m.chatterSay(ctx, sp, d.corpID, "social.greet", tmplID, text, chatterClassGreet, chatterSayBudget)
	}
}

func itoa64(n int64) string { return itoa(int(n)) }

// chatterGreetedElsewhere reports whether a person was greeted (or is about
// to be) in a corporation channel within the hour.
func chatterGreetedElsewhere(humanID int64) bool {
	audienceMu.Lock()
	var corps []int64
	for corpID, a := range audiences {
		if !a.Alliance && a.Humans[humanID] != "" {
			corps = append(corps, corpID)
		}
	}
	for k := range pendingGreet {
		if _, h := splitChatterKey(k); h == humanID {
			audienceMu.Unlock()
			return true
		}
	}
	audienceMu.Unlock()
	for _, corpID := range corps {
		if chatterBook.greetedWithin(clock.Now(), corpID, humanID, time.Hour) {
			return true
		}
	}
	return false
}

// splitChatterKey undoes chatterKey for a "corp|human" greeting key.
func splitChatterKey(k string) (int64, int64) {
	var a, b int64
	i := 0
	for ; i < len(k) && k[i] != '|'; i++ {
		a = a*10 + int64(k[i]-'0')
	}
	for i++; i < len(k); i++ {
		b = b*10 + int64(k[i]-'0')
	}
	return a, b
}

func init() {
	registerCorpForget("audiences", func(id int64) {
		audienceMu.Lock()
		delete(audiences, id)
		prefix := itoa64(id) + "|"
		for k := range pendingGreet {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				delete(pendingGreet, k)
			}
		}
		audienceMu.Unlock()
	})
}
