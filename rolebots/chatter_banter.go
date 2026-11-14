package rolebots

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Banter: pilots of different peoples passing the time. Two pilots of different races share
// a room (Local of a system a person is in, or an alliance channel) and one says something
// of its own people's ways; the other answers from where it stands: a xenophile asks, a
// xenophobe is cold, most are polite. Sometimes the first has the last word.
// The topic is the opener's people, never a voice. Local keeps the race rules (a far-gone
// xenophobe may insult when "Race insults" is on); an alliance channel never does (chatSay
// drops the slot). Its own lane paces it, one exchange per channel per banterMin minutes at
// most, spending the channel's ordinary budget, so a fight leaves no room for it.

const (
	banterEveryTicks   = 60
	banterPerPass      = 3
	banterCareerOneIn  = 3 // alliance channels: one turn in three is shop talk
	banterCloserOdds   = 40
	banterReplyOddsPct = 85
)

// banterTopic is the opener's people as a pool key.
func banterTopic(race int64) string {
	switch race {
	case raceAmarr:
		return "amarr"
	case raceCaldari:
		return "caldari"
	case raceGallente:
		return "gallente"
	case raceMinmatar:
		return "minmatar"
	}
	return "home"
}

// banterTopics is every topic a pool is written for.
var banterTopics = []string{"amarr", "caldari", "gallente", "minmatar", "home"}

func banterOpen(race int64) chatEvent  { return chatEvent("banteropen:" + banterTopic(race)) }
func banterReply(race int64) chatEvent { return chatEvent("banterreply:" + banterTopic(race)) }
func banterClose(race int64) chatEvent { return chatEvent("banterclose:" + banterTopic(race)) }

func isBanterEvent(ev chatEvent) bool { return strings.HasPrefix(string(ev), "banter") }

// toneScorn is a far-gone xenophobe answering another people in Local: harsher than cold,
// never heard in an alliance channel.
const toneScorn motive = "scorn"

// banterTone is how a pilot answers another people's talk: scorn when its
// prejudice shuts them out (cold instead among allies, who keep a floor of
// civility), cold when its corporation thinks poorly of theirs, curious when
// it is a xenophile, polite otherwise (warm among friends).
func banterTone(self, other *chatParty, ally bool) motive {
	if self == nil || other == nil {
		return tonePolite
	}
	if relCorpStanding(self.CorpID, other.CorpID) <= StandingDisliked {
		return toneCold
	}
	foreign := self.RaceID != 0 && other.RaceID != 0 && self.RaceID != other.RaceID
	if foreign {
		switch {
		case xenoPhobeStage(self.Phobia) >= xenoStageOwnRace:
			if ally {
				return toneCold
			}
			return toneScorn
		case self.Philia >= xenoWary:
			return toneCurious
		}
		return tonePolite
	}
	if relCorpStanding(self.CorpID, other.CorpID) >= StandingLiked {
		return toneWarm
	}
	return tonePolite
}

var (
	banterMu   sync.Mutex
	banterNext = map[int64]int64{} // channel → unix when it may banter again
	banterBusy atomic.Bool
)

func (m *Mod) startBanterChatter() {
	m.registerPeriodic("rolebots-chatter-banter", banterEveryTicks, func(uint64) bool {
		if !Master() || !Chatter() {
			return true
		}
		if !banterBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("chatter banter", func() {
			defer banterBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), chatterLaneTimeout)
			defer cancel()
			m.banterPass(ctx)
		})
		return true
	})
}

// banterSeat is one bot as the lane sees it.
type banterSeat struct {
	CharID, CorpID, Race int64
}

// banterRooms is every channel that could banter right now, with its bots:
// Local of a system a person is in, an alliance channel a person reads.
func (m *Mod) banterRooms() map[int64][]banterSeat {
	local := chatterBanterLocalOn.Load() && chatterLocalOn.Load() && m.lsc != nil
	ally := chatterBanterAllyOn.Load() && chatterAllianceOn.Load()
	rooms := map[int64][]banterSeat{}
	presenceMu.Lock()
	for _, r := range presenceApplied {
		seat := banterSeat{CharID: r.CharID, CorpID: r.CorpID, Race: raceOf(r.CharID)}
		if seat.Race == 0 {
			continue
		}
		if local && r.SolarSystemID != 0 {
			rooms[r.SolarSystemID] = append(rooms[r.SolarSystemID], seat)
		}
		if ally && r.AllianceID != 0 {
			rooms[r.AllianceID] = append(rooms[r.AllianceID], seat)
		}
	}
	presenceMu.Unlock()
	for ch := range rooms {
		hear := false
		if isAllianceChannel(ch) {
			hear = chatterWatched(ch)
		} else {
			hear = m.lsc.HasLiveListener(ch)
		}
		if !hear {
			delete(rooms, ch)
		}
	}
	return rooms
}

// banterDue reports whether a channel may banter now, and books its next
// turn when it may. A channel seen for the first time waits one interval: a
// person who just logged in hears hellos first, not a lecture on the tribes.
func banterDue(ch, now int64) bool {
	every := chatterBanterMin.Load() * 60
	next := now + int64(float64(every)*(0.7+0.6*rand.Float64()))
	banterMu.Lock()
	defer banterMu.Unlock()
	at, seen := banterNext[ch]
	if !seen || at > now {
		if !seen {
			banterNext[ch] = next
		}
		return false
	}
	banterNext[ch] = next
	return true
}

// banterPair picks two pilots of different peoples (and, in an alliance
// channel, different corporations). Pure but for the shuffle.
func banterPair(seats []banterSeat, ally bool) (banterSeat, banterSeat, bool) {
	s := append([]banterSeat(nil), seats...)
	rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
	for i := range s {
		for j := range s {
			if i == j || s[i].Race == s[j].Race {
				continue
			}
			if ally && s[i].CorpID == s[j].CorpID {
				continue
			}
			return s[i], s[j], true
		}
	}
	return banterSeat{}, banterSeat{}, false
}

// banterPass is one minute of the lane.
func (m *Mod) banterPass(ctx context.Context) {
	rooms := m.banterRooms()
	chans := make([]int64, 0, len(rooms))
	for ch := range rooms {
		chans = append(chans, ch)
	}
	sort.Slice(chans, func(i, j int) bool { return chans[i] < chans[j] })
	rand.Shuffle(len(chans), func(i, j int) { chans[i], chans[j] = chans[j], chans[i] })
	now := clock.Unix()
	said := 0
	for _, ch := range chans {
		if said >= banterPerPass || ctx.Err() != nil {
			return
		}
		if !banterDue(ch, now) {
			continue
		}
		ally := isAllianceChannel(ch)
		if ally && rand.Intn(banterCareerOneIn) == 0 && m.allyCareerSay(ctx, ch) {
			said++
			continue
		}
		a, b, ok := banterPair(rooms[ch], ally)
		if !ok {
			continue
		}
		if m.banterSay(ctx, ch, ally, a, b) {
			said++
		}
	}
}

// banterSay runs one exchange between two seated pilots.
func (m *Mod) banterSay(ctx context.Context, ch int64, ally bool, a, b banterSeat) bool {
	opener := m.corpParty(ctx, a.CharID, a.CorpID)
	other := m.corpParty(ctx, b.CharID, b.CorpID)
	if opener == nil || other == nil || opener.Voice == voicePirate || other.Voice == voicePirate {
		return false
	}
	tone := banterTone(other, opener, ally)
	m.runExchange("banter", ch, ally, []beat{
		{after: 2 * time.Second, speaker: opener, about: other, ev: banterOpen(opener.RaceID)},
		{after: 7 * time.Second, speaker: other, about: opener, ev: banterReply(opener.RaceID), why: tone,
			odds: banterReplyOddsPct},
		{after: 6 * time.Second, speaker: opener, about: other, ev: banterClose(opener.RaceID), why: tone,
			odds: banterCloserOdds},
	})
	return true
}

func init() {
	registerSweep("chatterBanter", func() {
		now := clock.Unix()
		banterMu.Lock()
		for ch, at := range banterNext {
			if now-at > 6*3600 {
				delete(banterNext, ch)
			}
		}
		banterMu.Unlock()
	})
}
