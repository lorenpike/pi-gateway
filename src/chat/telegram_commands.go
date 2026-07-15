package chat

import (
	"sort"
	"strings"

	"wall-e/rpc"
)

const (
	telegramCommandMaxLen         = 32
	telegramCommandDescriptionMax = 256
	telegramCommandRegisterMax    = 100
)

type telegramCommand struct {
	TelegramName string
	PiName       string
	Source       string
	Description  string
}

type telegramSkill struct {
	Name        string
	PiName      string
	Description string
}

type telegramCommandRegistry struct {
	byTelegram  map[string]telegramCommand
	skillsByKey map[string]telegramSkill
	skills      []telegramSkill
	all         []telegramCommand
	registered  []BotCommand
}

var telegramNativeCommands = func() []telegramCommand {
	out := make([]telegramCommand, 0, len(gatewayNativeCommands))
	for _, command := range gatewayNativeCommands {
		out = append(out, telegramCommand{TelegramName: command.Name, Source: "gateway", Description: command.Description})
	}
	return out
}()

func newTelegramCommandRegistry(commands []rpc.Command) *telegramCommandRegistry {
	r := &telegramCommandRegistry{
		byTelegram:  make(map[string]telegramCommand),
		skillsByKey: make(map[string]telegramSkill),
	}
	used := make(map[string]bool)
	for _, native := range telegramNativeCommands {
		desc := truncateRunes(native.Description, telegramCommandDescriptionMax)
		native.Description = desc
		r.byTelegram[native.TelegramName] = native
		r.all = append(r.all, native)
		used[native.TelegramName] = true
		if len(r.registered) < telegramCommandRegisterMax {
			r.registered = append(r.registered, BotCommand{Command: native.TelegramName, Description: desc})
		}
	}

	for _, cmd := range commands {
		if cmd.Source == "skill" || strings.HasPrefix(cmd.Name, "skill:") {
			r.addSkill(cmd)
			continue
		}
		base := telegramAliasBase(cmd.Name)
		if base == "" {
			continue
		}
		alias := uniqueTelegramAlias(base, used)
		if alias == "" {
			continue
		}
		used[alias] = true
		desc := commandDescription(cmd)
		tc := telegramCommand{
			TelegramName: alias,
			PiName:       cmd.Name,
			Source:       cmd.Source,
			Description:  desc,
		}
		r.byTelegram[alias] = tc
		r.all = append(r.all, tc)
		if len(r.registered) < telegramCommandRegisterMax {
			r.registered = append(r.registered, BotCommand{Command: alias, Description: desc})
		}
	}
	sort.SliceStable(r.skills, func(i, j int) bool { return r.skills[i].Name < r.skills[j].Name })
	return r
}

func (r *telegramCommandRegistry) addSkill(cmd rpc.Command) {
	name := strings.TrimPrefix(cmd.Name, "skill:")
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	skill := telegramSkill{Name: name, PiName: "skill:" + name, Description: commandDescription(cmd)}
	if _, exists := r.skillsByKey[strings.ToLower(name)]; !exists {
		r.skills = append(r.skills, skill)
	}
	r.skillsByKey[strings.ToLower(name)] = skill
	alias := telegramAliasBase(name)
	if alias != "" {
		r.skillsByKey[alias] = skill
	}
}

func (r *telegramCommandRegistry) lookup(alias string) (telegramCommand, bool) {
	if r == nil {
		return telegramCommand{}, false
	}
	cmd, ok := r.byTelegram[alias]
	return cmd, ok
}

func (r *telegramCommandRegistry) lookupSkill(name string) (telegramSkill, bool) {
	if r == nil {
		return telegramSkill{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return telegramSkill{}, false
	}
	if skill, ok := r.skillsByKey[strings.ToLower(name)]; ok {
		return skill, true
	}
	if skill, ok := r.skillsByKey[telegramAliasBase(name)]; ok {
		return skill, true
	}
	return telegramSkill{}, false
}

func (r *telegramCommandRegistry) botCommands() []BotCommand {
	if r == nil || len(r.registered) == 0 {
		return nil
	}
	out := make([]BotCommand, len(r.registered))
	copy(out, r.registered)
	return out
}

func (r *telegramCommandRegistry) skillListText() string {
	if r == nil || len(r.skills) == 0 {
		return "No pi skills are available."
	}
	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, s := range r.skills {
		b.WriteString("/skill ")
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(" — ")
			b.WriteString(s.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func telegramAliasBase(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if valid {
			b.WriteRune(r)
			lastUnderscore = r == '_'
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	alias := strings.Trim(b.String(), "_")
	return truncateTelegramCommand(alias)
}

func uniqueTelegramAlias(base string, used map[string]bool) string {
	if base == "" {
		return ""
	}
	if !used[base] {
		return base
	}
	for i := 2; i < 10000; i++ {
		suffix := "_" + itoaSmall(i)
		stemLen := telegramCommandMaxLen - len(suffix)
		if stemLen <= 0 {
			return ""
		}
		stem := truncateTelegramCommand(base)
		if len(stem) > stemLen {
			stem = stem[:stemLen]
			stem = strings.TrimRight(stem, "_")
		}
		candidate := stem + suffix
		if candidate != "" && !used[candidate] {
			return candidate
		}
	}
	return ""
}

func truncateTelegramCommand(s string) string {
	if len(s) <= telegramCommandMaxLen {
		return s
	}
	return strings.TrimRight(s[:telegramCommandMaxLen], "_")
}

func commandDescription(cmd rpc.Command) string {
	desc := strings.TrimSpace(cmd.Description)
	if desc == "" {
		source := strings.TrimSpace(cmd.Source)
		if source == "" {
			source = "pi"
		}
		desc = "Pi " + source + " command"
	}
	desc = strings.Join(strings.Fields(desc), " ")
	return truncateRunes(desc, telegramCommandDescriptionMax)
}

func parseTelegramCommandText(text, botName string) (name, args string, isSlash, addressedToOtherBot bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false, false
	}
	first, rest, _ := strings.Cut(text, " ")
	if first == "/" {
		return "", "", false, false
	}
	cmd := strings.TrimPrefix(first, "/")
	if cmd == "" {
		return "", "", false, false
	}
	if before, after, ok := strings.Cut(cmd, "@"); ok {
		if botName == "" || !strings.EqualFold(after, botName) {
			return strings.ToLower(before), rest, true, true
		}
		cmd = before
	}
	return strings.ToLower(cmd), rest, true, false
}

func rewriteTelegramCommandText(text, botName string, registry *telegramCommandRegistry) (string, bool, bool) {
	cmd, rest, isSlash, other := parseTelegramCommandText(text, botName)
	if !isSlash || other {
		return text, isSlash, other
	}
	if tc, ok := registry.lookup(cmd); ok && tc.Source != "gateway" {
		if rest == "" {
			return "/" + tc.PiName, true, false
		}
		return "/" + tc.PiName + " " + rest, true, false
	}
	return text, true, false
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
