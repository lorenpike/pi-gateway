package chat

import (
	"sort"
	"strings"

	"wall-e/rpc"
)

const (
	discordCommandMaxLen         = 32
	discordCommandDescriptionMax = 100
	discordCommandRegisterMax    = 100
	discordCommandOptionMax      = 6000
)

type discordRegisteredCommand struct {
	Alias       string
	PiName      string
	Source      string
	Description string
}

type discordCommandRegistry struct {
	byAlias map[string]discordRegisteredCommand
	skills  []telegramSkill
	skillBy map[string]telegramSkill
	catalog []DiscordCommand
}

func newDiscordCommandRegistry(commands []rpc.Command) *discordCommandRegistry {
	r := &discordCommandRegistry{byAlias: make(map[string]discordRegisteredCommand), skillBy: make(map[string]telegramSkill)}
	used := make(map[string]bool)
	for _, native := range gatewayNativeCommands {
		command := discordRegisteredCommand{Alias: native.Name, Source: "gateway", Description: discordDescription(native.Description)}
		r.byAlias[command.Alias] = command
		used[command.Alias] = true
		r.catalog = append(r.catalog, discordNativeCommand(command))
	}
	discovered := append([]rpc.Command(nil), commands...)
	sort.SliceStable(discovered, func(i, j int) bool {
		if discovered[i].Name != discovered[j].Name {
			return discovered[i].Name < discovered[j].Name
		}
		return discovered[i].Source < discovered[j].Source
	})
	for _, command := range discovered {
		if command.Source == "skill" || strings.HasPrefix(command.Name, "skill:") {
			name := strings.TrimSpace(strings.TrimPrefix(command.Name, "skill:"))
			if name == "" {
				continue
			}
			skill := telegramSkill{Name: name, PiName: "skill:" + name, Description: discordDescription(commandDescription(command))}
			key := strings.ToLower(name)
			if _, exists := r.skillBy[key]; !exists {
				r.skills = append(r.skills, skill)
			}
			r.skillBy[key] = skill
			if alias := telegramAliasBase(name); alias != "" {
				r.skillBy[alias] = skill
			}
			continue
		}
		base := telegramAliasBase(command.Name)
		alias := uniqueTelegramAlias(base, used)
		if alias == "" {
			continue
		}
		used[alias] = true
		registered := discordRegisteredCommand{Alias: alias, PiName: command.Name, Source: command.Source, Description: discordDescription(commandDescription(command))}
		r.byAlias[alias] = registered
		if len(r.catalog) < discordCommandRegisterMax {
			r.catalog = append(r.catalog, DiscordCommand{Name: alias, Description: registered.Description, Options: []DiscordCommandOption{{Name: "args", Description: "Arguments passed to the pi command", MaxLength: discordCommandOptionMax}}})
		}
	}
	sort.SliceStable(r.skills, func(i, j int) bool { return r.skills[i].Name < r.skills[j].Name })
	return r
}

func discordNativeCommand(command discordRegisteredCommand) DiscordCommand {
	out := DiscordCommand{Name: command.Alias, Description: command.Description}
	switch command.Alias {
	case "skill":
		out.Options = []DiscordCommandOption{
			{Name: "name", Description: "Skill name; omit to list skills", MaxLength: discordCommandOptionMax},
			{Name: "args", Description: "Arguments passed to the skill", MaxLength: discordCommandOptionMax},
		}
	case "name":
		out.Options = []DiscordCommandOption{{Name: "value", Description: "New session name; omit to clear", MaxLength: discordCommandOptionMax}}
	case "compact":
		out.Options = []DiscordCommandOption{{Name: "instructions", Description: "Optional compaction instructions", MaxLength: discordCommandOptionMax}}
	}
	return out
}

func discordDescription(description string) string {
	description = strings.Join(strings.Fields(strings.TrimSpace(description)), " ")
	if description == "" {
		description = "Pi command"
	}
	return truncateRunes(description, discordCommandDescriptionMax)
}

func (r *discordCommandRegistry) lookup(alias string) (discordRegisteredCommand, bool) {
	if r == nil {
		return discordRegisteredCommand{}, false
	}
	command, ok := r.byAlias[strings.ToLower(alias)]
	return command, ok
}

func (r *discordCommandRegistry) lookupSkill(name string) (telegramSkill, bool) {
	if r == nil {
		return telegramSkill{}, false
	}
	name = strings.TrimSpace(name)
	if skill, ok := r.skillBy[strings.ToLower(name)]; ok {
		return skill, true
	}
	skill, ok := r.skillBy[telegramAliasBase(name)]
	return skill, ok
}

func (r *discordCommandRegistry) skillListText() string {
	if r == nil || len(r.skills) == 0 {
		return "No pi skills are available."
	}
	var builder strings.Builder
	builder.WriteString("Available skills:\n")
	for _, skill := range r.skills {
		builder.WriteString("/skill ")
		builder.WriteString(skill.Name)
		if skill.Description != "" {
			builder.WriteString(" — ")
			builder.WriteString(skill.Description)
		}
		builder.WriteByte('\n')
	}
	return strings.TrimRight(builder.String(), "\n")
}

func (r *discordCommandRegistry) commands() []DiscordCommand {
	if r == nil {
		return nil
	}
	out := make([]DiscordCommand, len(r.catalog))
	copy(out, r.catalog)
	return out
}
