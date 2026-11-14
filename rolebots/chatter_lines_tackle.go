package rolebots

// The tackle calls (pointclaims.go pointCall): said in the pilot's real fleet
// chat only, while a human is aboard. {target} is the pointed ball's name, a
// link when it is a pilot.

const (
	evPointLanded chatEvent = "pointlanded" // this pilot's scram just landed
	evPointLost   chatEvent = "pointlost"   // this pilot's scram dropped off a live target
)

func init() {
	addLines(evPointLanded, motiveNone, voiceAny,
		"Point on {target}.",
		"{target} pointed.",
		"Got point on {target}.",
		"Scrammed {target}.",
		"Tackled: {target}.",
	)
	addLines(evPointLanded, motiveNone, voiceTerse,
		"Point {target}.",
		"{target}: pointed.",
	)
	addLines(evPointLanded, motiveNone, voiceCold,
		"{target} is held.",
		"Point on {target}. It stays.",
	)
	addLines(evPointLanded, motiveNone, voiceBrash,
		"Got {target} pointed, pile in!",
		"{target}'s not going anywhere. Point!",
	)
	addLines(evPointLanded, motiveNone, voiceCorporate,
		"Point established on {target}.",
	)
	addLines(evPointLanded, motiveNone, voiceZealot,
		"{target} is bound. Point.",
	)
	addLines(evPointLanded, motiveNone, voiceShowman,
		"And {target} is pinned! Point!",
	)
	addLines(evPointLanded, motiveNone, voiceTribal,
		"{target} caught. Point on.",
	)
	addLines(evPointLanded, motiveNone, voicePirate,
		"{target}'s going nowhere. Point.",
		"Hooked {target}. Point.",
	)

	addLines(evPointLost, motiveNone, voiceAny,
		"Lost point on {target}.",
		"Point dropped on {target}.",
		"{target} is off my point.",
	)
	addLines(evPointLost, motiveNone, voiceTerse,
		"Lost {target}.",
	)
	addLines(evPointLost, motiveNone, voiceBrash,
		"Lost point on {target}, someone grab it!",
	)
	addLines(evPointLost, motiveNone, voicePirate,
		"{target} slipped the hook. Lost point.",
	)
}
