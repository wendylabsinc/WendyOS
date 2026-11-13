package data

import (
	"fmt"
	"strings"
)

// LegacyAudioSourcePrefix starts the microphone identifiers agents listed
// before microphones had stable IDs: "audio:" and a number.
const LegacyAudioSourcePrefix = "audio:"

// IsLegacyAudioSourceID reports whether id has the old microphone form,
// "audio:<n>".
//
// The number was never an identity. On a PipeWire host it is the node's
// object.id, a slot PipeWire's pw_map hands out from a free list that reuses
// the most recently released index, so a replugged microphone usually comes
// back under another number and a reboot renumbers every node in registration
// order. On the ALSA fallback it packs the card index, which the kernel assigns
// in probe order. And the same microphone was listed under unrelated numbers on
// the two paths, so whether the PipeWire session was up when the agent looked
// changed it too.
//
// A stable ID never matches: every form the audio adapter mints has a
// non-digit after the prefix.
func IsLegacyAudioSourceID(id string) bool {
	n, ok := strings.CutPrefix(id, LegacyAudioSourcePrefix)
	if !ok || n == "" {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// legacyAudioIDHint is appended where a legacy microphone ID fails to resolve,
// because the bare "unknown source" that would otherwise follow does not tell
// anyone the ID scheme moved.
func legacyAudioIDHint(id string) string {
	return fmt.Sprintf("%s is an old-style microphone ID, a session-local number that can change after a reboot or a replug; `wendy data sources --kind audio` lists the stable IDs", id)
}

// aliasTarget returns the source that lists alias among its Aliases. An
// identifier that is some source's own ID is never treated as an alias.
func aliasTarget(all []Source, alias string) (Source, bool) {
	for _, source := range all {
		if source.ID == alias {
			return Source{}, false
		}
	}
	for _, source := range all {
		for _, a := range source.Aliases {
			if a == alias {
				return source, true
			}
		}
	}
	return Source{}, false
}

// resolveSourceAliases rewrites every requested identifier that is a source's
// alias, rather than its ID, to that source's ID. It is applied to everything a
// person types as a source ID: `wendy data record --source`, `--exclude-source`
// and the source half of `--calibration`. Campaign selectors resolve the same
// aliases through resolveKindSelector.
//
// It returns one note per rewrite, which the caller logs: the alias still works
// so nothing breaks, but the person who wrote it should move to the ID it
// resolved to, because the alias stops meaning that microphone as soon as the
// microphone is replugged.
func resolveSourceAliases(all []Source, ids []string) ([]string, []string) {
	if len(ids) == 0 {
		return ids, nil
	}
	out := make([]string, 0, len(ids))
	var notes []string
	for _, id := range ids {
		target, ok := aliasTarget(all, id)
		if !ok {
			out = append(out, id)
			continue
		}
		out = append(out, target.ID)
		notes = append(notes, fmt.Sprintf("source %s is deprecated and resolved to %s; it is a session-local number that can change after a reboot or a replug, so select %s instead", id, target.ID, target.ID))
	}
	return out, notes
}

// resolveCalibrationAliases is resolveSourceAliases for the keys of a
// calibration map, so `--calibration audio:<n>=file` lands on the source the
// episode actually records and is associated with its files.
func resolveCalibrationAliases(all []Source, calibrations map[string][]byte) (map[string][]byte, []string) {
	if len(calibrations) == 0 {
		return calibrations, nil
	}
	var notes []string
	out := make(map[string][]byte, len(calibrations))
	for source, contents := range calibrations {
		resolved, rewritten := resolveSourceAliases(all, []string{source})
		notes = append(notes, rewritten...)
		out[resolved[0]] = contents
	}
	return out, notes
}
