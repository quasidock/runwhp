package container

import (
	"fmt"
	"math/rand/v2"
)

// Container names are made as Docker makes them, an adjective and a noun
// joined by an underscore, from word lists of runwhp's own.
var (
	nameAdjectives = []string{
		"agile", "amber", "ample", "azure", "bold", "brave", "breezy", "bright", "brisk", "calm",
		"candid", "clever", "cosmic", "crisp", "curious", "dapper", "daring", "deft", "eager", "earnest",
		"fair", "fancy", "fearless", "fond", "frank", "gentle", "glad", "golden", "graceful", "hardy",
		"hearty", "humble", "jolly", "keen", "kind", "lively", "lucid", "lucky", "mellow", "merry",
		"mighty", "modest", "nimble", "noble", "patient", "plucky", "polite", "proud", "quick", "quiet",
		"radiant", "rapid", "ready", "serene", "sharp", "shy", "silent", "sleek", "snappy", "spry",
		"steady", "sturdy", "sunny", "swift", "tender", "tidy", "tranquil", "trusty", "vivid", "warm",
		"wise", "witty", "zealous", "zesty",
	}
	nameNouns = []string{
		"acorn", "alder", "aspen", "badger", "basil", "beacon", "birch", "bison", "brook", "canyon",
		"cedar", "comet", "condor", "coral", "cove", "crane", "dune", "eagle", "ember", "falcon",
		"fern", "fjord", "finch", "fox", "galaxy", "garnet", "glacier", "grove", "harbor", "hawk",
		"heron", "hollow", "ibis", "island", "jasper", "juniper", "kestrel", "lagoon", "lark", "lynx",
		"maple", "marsh", "meadow", "mesa", "nebula", "oak", "orca", "osprey", "otter", "owl",
		"pebble", "pine", "plover", "prairie", "quartz", "raven", "reef", "ridge", "river", "robin",
		"sage", "sparrow", "spruce", "summit", "swallow", "thistle", "tundra", "valley", "walnut", "willow",
		"wren", "yarrow",
	}
)

// NewName makes a name for a container given none, as Docker does:
// adjective_noun, with a digit after it on a retry, as Docker adds one.
func NewName(retry int) string {
	name := nameAdjectives[rand.IntN(len(nameAdjectives))] + "_" + nameNouns[rand.IntN(len(nameNouns))]
	if retry > 0 {
		name += fmt.Sprint(rand.IntN(10))
	}
	return name
}
