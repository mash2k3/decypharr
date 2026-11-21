package manager

import (
	"path"
	"regexp"
	"strings"

	"github.com/sirrobot01/decypharr/internal/utils"
)

// Extras naming. A media server pointed straight at the mount (cli_debrid's
// Plex mode) sees an entry's whole folder: a movie torrent's bonus videos
// (Inception.Extras1...mkv) then become extra *versions* of the movie, and
// Play can pick one of them instead of the film. Plex and Jellyfin both read
// a "-featurette"/"-trailer"/... suffix as "this is an extra", so the mount
// lists such files with that suffix. Only the listed name changes: lookups by
// the real name still work (existing symlinks), and storage, streaming and
// repair keep using the real name. Same rules as cli_debrid's built-in mount.

var (
	reEpisodeName = regexp.MustCompile(`(?i)(^|[^a-z0-9])(s\d{1,3}[ ._-]?e\d{1,3}|\d{1,2}x\d{2})([^0-9]|$)`)
	reExtraSuffix = regexp.MustCompile(`(?i)-(trailer|deleted|behindthescenes|featurette|interview|scene|short|other)$`)
	extraKinds    = []struct {
		re     *regexp.Regexp
		suffix string
	}{
		{regexp.MustCompile(`(?i)(^|[^a-z])(trailers?|teasers?)([^a-z]|$)`), "trailer"},
		{regexp.MustCompile(`(?i)(^|[^a-z])deleted[ ._-]?scenes?([^a-z]|$)`), "deleted"},
		{regexp.MustCompile(`(?i)(^|[^a-z])(behind[ ._-]?the[ ._-]?scenes|making[ ._-]?of)([^a-z]|$)`), "behindthescenes"},
		{regexp.MustCompile(`(?i)(^|[^a-z])interviews?([^a-z]|$)`), "interview"},
		{regexp.MustCompile(`(?i)(^|[^a-z])(featurettes?|extras?\d*|bonus|special[ ._-]?features?)([^a-z]|$)`), "featurette"},
		{regexp.MustCompile(`(?i)(^|[^a-z])(gag[ ._-]?reels?|bloopers?|outtakes?)([^a-z]|$)`), "other"},
	}
)

// extraOtherShare: a video without an extras word still counts as an extra
// of a movie when it is below this share of the main file's size.
const extraOtherShare = 0.2

type namedFile struct {
	name string
	size int64
}

// extraNames returns real name -> listed name for each extra video in an
// entry (only the renamed ones). It never renames the main (largest) video
// or an episode, and leaves single-video entries alone.
func extraNames(files []namedFile) map[string]string {
	main := -1
	videos := 0
	for i, f := range files {
		if !utils.IsMediaFile(f.name) {
			continue
		}
		videos++
		if main < 0 || f.size > files[main].size {
			main = i
		}
	}
	if videos < 2 {
		return nil
	}
	mainName := path.Base(files[main].name)
	mainBase := strings.TrimSuffix(mainName, path.Ext(mainName))
	movie := !reEpisodeName.MatchString(mainName)
	taken := make(map[string]bool, len(files))
	for _, f := range files {
		taken[strings.ToLower(f.name)] = true
	}
	var out map[string]string
	for i, f := range files {
		if i == main || !utils.IsMediaFile(f.name) || reEpisodeName.MatchString(path.Base(f.name)) {
			continue
		}
		ext := path.Ext(f.name)
		base := strings.TrimSuffix(f.name, ext)
		if reExtraSuffix.MatchString(base) {
			continue
		}
		suffix := ""
		for _, k := range extraKinds {
			// A word the main file has too is part of the title ("Trailer
			// Park Boys"), not a sign of an extra.
			if k.re.MatchString(path.Base(base)) && !k.re.MatchString(mainBase) {
				suffix = k.suffix
				break
			}
		}
		if suffix == "" && movie && float64(f.size) < extraOtherShare*float64(files[main].size) {
			suffix = "other"
		}
		if suffix == "" {
			continue
		}
		name := base + "-" + suffix + ext
		if taken[strings.ToLower(name)] {
			continue
		}
		taken[strings.ToLower(name)] = true
		if out == nil {
			out = map[string]string{}
		}
		out[f.name] = name
	}
	return out
}
