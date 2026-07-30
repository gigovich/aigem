package bot

import (
	"embed"
	"fmt"
	"io/fs"
	"sync"

	"github.com/gigovich/aigem/internal/skill"
)

//go:embed skills
var systemSkillsFS embed.FS

// SystemSkills returns the registry of skills embedded in the binary. Their names are
// reserved: a bot's self-authored skill cannot claim or delete them. A fresh registry is
// built per call - do not memoize it, MergeMissing mutates its receiver and a shared
// registry would leak one bot's self-skills into every later discovery.
func SystemSkills() (*skill.Registry, []error) {
	sub, err := fs.Sub(systemSkillsFS, "skills")
	if err != nil {
		// go:embed guarantees the skills directory exists at compile time.
		panic(err)
	}
	return skill.DiscoverFS(sub)
}

var systemSkillNames = sync.OnceValue(func() []string {
	reg, _ := SystemSkills()
	var out []string
	for _, s := range reg.List() {
		out = append(out, s.Name)
	}
	return out
})

// SystemSkillNames returns the reserved built-in skill names.
func SystemSkillNames() []string { return systemSkillNames() }

// DiscoverBotSkills merges the embedded system skills, which claim their names first, with
// the bot's self-authored skills from skillsDir. A self-authored skill left on disk under a
// reserved name (saved before the name became reserved) is excluded and reported as an
// error; delete_skill can still remove it.
func DiscoverBotSkills(skillsDir string) (*skill.Registry, []error) {
	reg, errs := SystemSkills()
	selfSkills, selfErrs := skill.DiscoverDir(skillsDir)
	errs = append(errs, selfErrs...)
	for _, s := range selfSkills.List() {
		if reservedSkillSlug(slugify(s.Name)) {
			selfSkills.Remove(s.Name)
			errs = append(errs, fmt.Errorf(
				"self-authored skill %q is shadowed by the built-in skill and ignored; "+
					"delete it with delete_skill", s.Name))
		}
	}
	reg.MergeMissing(selfSkills)
	return reg, errs
}
