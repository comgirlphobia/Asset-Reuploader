package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kartFr/Asset-Reuploader/internal/app/config"
	"github.com/kartFr/Asset-Reuploader/internal/app/request"
	"github.com/kartFr/Asset-Reuploader/internal/files"
	"github.com/kartFr/Asset-Reuploader/internal/roblox"
	"github.com/kartFr/Asset-Reuploader/internal/server"
)

type GuildDefaults struct {
	PlaceID   int64 `json:"placeId"`
	CreatorID int64 `json:"creatorId"`
	IsGroup   bool  `json:"isGroup"`
}

var defaultsPath = "bot_defaults.json"
var guildDefaults = map[string]GuildDefaults{}

func loadDefaults() {
	if b, err := os.ReadFile(defaultsPath); err == nil {
		_ = json.Unmarshal(b, &guildDefaults)
	}
}

func saveDefaults() {
	b, _ := json.MarshalIndent(guildDefaults, "", "  ")
	_ = os.WriteFile(defaultsPath, b, 0644)
}

func main() {
	botToken := os.Getenv("DISCORD_TOKEN")
	if botToken == "" {
		// Optional: allow putting token in config.ini as discord_token
		botToken = os.Getenv("ASSET_REUPLOADER_DISCORD_TOKEN")
	}
	if botToken == "" {
		fmt.Println("Missing DISCORD_TOKEN environment variable. Set it to your bot token and rerun.")
		return
	}

	port := config.Get("port")
	apiBase := fmt.Sprintf("http://localhost:%s", port)

	// Authenticate Roblox client and start HTTP server in the same process
	cookieFile := config.Get("cookie_file")
	cookie, _ := files.Read(cookieFile)
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		cookie = strings.TrimSpace(os.Getenv("ROBLOSECURITY"))
	}
	c, err := roblox.NewClient(cookie)
	if err != nil || strings.TrimSpace(c.Cookie) == "" {
		fmt.Println("Invalid or missing ROBLOSECURITY. Put it in", cookieFile, "or set ROBLOSECURITY env and rerun.")
		return
	}
	_ = files.Write(cookieFile, c.Cookie)
	go func() {
		if err := server.Start(c); err != nil {
			fmt.Println("HTTP server error:", err)
		}
	}()

	loadDefaults()

	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		fmt.Println("Failed to create Discord session:", err)
		return
	}

	// Slash commands
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}

		switch i.ApplicationCommandData().Name {
		case "status":
			status := fetchStatus(apiBase)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: status, Flags: 1 << 6},
			})
		case "reupload":
			opts := i.ApplicationCommandData().Options
			// defaults
			var rr request.RawRequest
			rr.PluginVersion = "discord-bot"
			// Load guild defaults
			gd := guildDefaults[i.GuildID]
			rr.PlaceID = gd.PlaceID
			rr.CreatorID = gd.CreatorID
			rr.IsGroup = gd.IsGroup
			for _, opt := range opts {
				switch opt.Name {
				case "asset_type":
					rr.AssetType = opt.StringValue()
				case "ids":
					ids := parseIDsFromString(opt.StringValue())
					rr.IDs = ids
				case "place_id":
					rr.PlaceID = int64(opt.IntValue())
				case "creator_id":
					rr.CreatorID = int64(opt.IntValue())
				case "is_group":
					rr.IsGroup = opt.BoolValue()
				case "export_json":
					rr.ExportJSON = opt.BoolValue()
				}
			}

			if rr.AssetType == "" || len(rr.IDs) == 0 || rr.PlaceID == 0 || rr.CreatorID == 0 {
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{Content: "Missing required fields. Provide asset_type, ids, and ensure defaults are set or pass place_id + creator_id.", Flags: 1 << 6},
				})
				return
			}

			// Kick off reupload
			b, _ := json.Marshal(rr)
			resp, err := http.Post(apiBase+"/reupload", "application/json", bytes.NewReader(b))
			if err != nil || resp.StatusCode != http.StatusOK {
				var status int
				if resp != nil { status = resp.StatusCode }
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{Content: fmt.Sprintf("Failed to start reupload (status %d): %v", status, err), Flags: 1 << 6},
				})
				return
			}
			if resp != nil { resp.Body.Close() }

			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "Reupload started. I will post progress here.", Flags: 1 << 6},
			})

			go func(channelID string) {
				pollProgress(s, channelID, apiBase)
			}(i.ChannelID)
		case "setdefault":
			opts := i.ApplicationCommandData().Options
			gd := guildDefaults[i.GuildID]
			for _, opt := range opts {
				switch opt.Name {
				case "place_id":
					gd.PlaceID = int64(opt.IntValue())
				case "creator_id":
					gd.CreatorID = int64(opt.IntValue())
				case "is_group":
					gd.IsGroup = opt.BoolValue()
				}
			}
			guildDefaults[i.GuildID] = gd
			saveDefaults()
			msg := fmt.Sprintf("Defaults saved. place_id=%d, creator_id=%d, is_group=%v", gd.PlaceID, gd.CreatorID, gd.IsGroup)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: msg, Flags: 1 << 6},
			})
		case "cleardefault":
			delete(guildDefaults, i.GuildID)
			saveDefaults()
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "Defaults cleared.", Flags: 1 << 6},
			})
		}
	})

	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}

		content := strings.TrimSpace(m.Content)
		if strings.HasPrefix(content, "!reupload ") {
			payload := strings.TrimSpace(strings.TrimPrefix(content, "!reupload "))
			var req request.RawRequest
			if err := json.Unmarshal([]byte(payload), &req); err != nil {
				_, _ = s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Invalid JSON: %v", err))
				return
			}

			// Provide a default plugin version marker for compatibility checks if configured.
			if req.PluginVersion == "" {
				req.PluginVersion = "discord-bot"
			}

			b, _ := json.Marshal(req)
			resp, err := http.Post(apiBase+"/reupload", "application/json", bytes.NewReader(b))
			if err != nil {
				_, _ = s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to reach reuploader server on %s: %v", apiBase, err))
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				msg := fmt.Sprintf("Server rejected request (status %d). Ensure the Asset Reuploader server is running and asset type is supported.", resp.StatusCode)
				_, _ = s.ChannelMessageSend(m.ChannelID, msg)
				return
			}

			_, _ = s.ChannelMessageSend(m.ChannelID, "Reupload started. Polling for progress...")

			go pollProgress(s, m.ChannelID, apiBase)
		} else if content == "!status" {
			status := fetchStatus(apiBase)
			_, _ = s.ChannelMessageSend(m.ChannelID, status)
		}
	})

	if err := dg.Open(); err != nil {
		fmt.Println("Failed to open Discord connection:", err)
		return
	}
	defer dg.Close()

	// Register slash commands
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "status",
			Description: "Show reuploader server status",
		},
		{
			Name:        "reupload",
			Description: "Reupload assets using simple inputs",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "asset_type",
					Description: "Type of asset (e.g., Animation)",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Animation", Value: "Animation"},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "ids",
					Description: "Comma/space separated IDs or Roblox URLs",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "place_id",
					Description: "Place ID for context",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "creator_id",
					Description: "Creator user/group ID",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "is_group",
					Description: "Set true if creator_id is a group",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "export_json",
					Description: "Export progress to JSON file on disk",
					Required:    false,
				},
			},
		},
		{
			Name:        "setdefault",
			Description: "Set default place/creator for this guild",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "place_id", Description: "Default place ID", Required: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "creator_id", Description: "Default creator ID", Required: true},
				{Type: discordgo.ApplicationCommandOptionBoolean, Name: "is_group", Description: "Creator is a group?", Required: true},
			},
		},
		{
			Name:        "cleardefault",
			Description: "Clear defaults for this guild",
		},
	}

	appID := dg.State.User.ID
	guildID := os.Getenv("DISCORD_GUILD_ID")
	for _, cmd := range commands {
		var _, regErr = dg.ApplicationCommandCreate(appID, guildID, cmd)
		if regErr != nil {
			fmt.Println("Failed to register command", cmd.Name, ":", regErr)
		}
	}

	fmt.Println("Discord bot is running. Commands: /reupload, /status, /setdefault, /cleardefault (also supports !reupload JSON, !status)")
	select {}
}

func pollProgress(s *discordgo.Session, channelID, apiBase string) {
	lastCount := 0
	for {
		res, body, err := get(apiBase + "/")
		if err != nil {
			_, _ = s.ChannelMessageSend(channelID, fmt.Sprintf("Error polling: %v", err))
			return
		}
		if res.StatusCode != http.StatusOK {
			_, _ = s.ChannelMessageSend(channelID, fmt.Sprintf("Poll status: %d", res.StatusCode))
			return
		}

		b := strings.TrimSpace(string(body))
		if b == "done" {
			_, _ = s.ChannelMessageSend(channelID, "Reupload finished.")
			return
		}

		// Expecting a JSON array of {oldId,newId}
		var items []map[string]interface{}
		if err := json.Unmarshal(body, &items); err == nil {
			if len(items) > 0 && len(items) != lastCount {
				lastCount = len(items)
				snippet := summarizeItems(items)
				_, _ = s.ChannelMessageSend(channelID, fmt.Sprintf("Progress: %d mappings so far. %s", len(items), snippet))
			}
		}

		time.Sleep(2 * time.Second)
	}
}

func summarizeItems(items []map[string]interface{}) string {
	max := 5
	if len(items) < max {
		max = len(items)
	}
	parts := make([]string, 0, max)
	for i := 0; i < max; i++ {
		oldV := items[i]["oldId"]
		newV := items[i]["newId"]
		parts = append(parts, fmt.Sprintf("%v→%v", oldV, newV))
	}
	if len(items) > max {
		parts = append(parts, fmt.Sprintf("(+%d more)", len(items)-max))
	}
	return strings.Join(parts, ", ")
}

func fetchStatus(apiBase string) string {
	res, body, err := get(apiBase + "/")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Sprintf("Server status: %d", res.StatusCode)
	}
	b := strings.TrimSpace(string(body))
	if b == "done" || b == "" {
		return "Idle"
	}
	var items []map[string]interface{}
	if err := json.Unmarshal(body, &items); err == nil {
		return fmt.Sprintf("In progress: %d items buffered", len(items))
	}
	return "Unknown server response"
}

func get(url string) (*http.Response, []byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return resp, nil, err
	}
	return resp, b, nil
}

var idMatcher = regexp.MustCompile(`\d+`)

func parseIDsFromString(s string) []int64 {
	parts := idMatcher.FindAllString(s, -1)
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}
