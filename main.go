package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// --- Global Variables & Constants ---
var (
	botToken          string
	guildID           string
	verifiedRoleID    string
	gmailAddress      string
	gmailAppPassword  string
	welcomeChannelID  string
	privateCategoryID string

	verificationCodes = make(map[string]string)
	codesMutex        = &sync.Mutex{}
)

const (
	startVerificationButtonID = "start_verification_button"
)

// --- Initialization ---
func init() {
	botToken = os.Getenv("DISCORD_BOT_TOKEN")
	guildID = os.Getenv("DISCORD_GUILD_ID")
	verifiedRoleID = os.Getenv("DISCORD_VERIFIED_ROLE_ID")
	gmailAddress = os.Getenv("GMAIL_ADDRESS")
	gmailAppPassword = os.Getenv("GMAIL_APP_PASSWORD")
	welcomeChannelID = os.Getenv("DISCORD_WELCOME_CHANNEL_ID")
	privateCategoryID = os.Getenv("DISCORD_PRIVATE_CATEGORY_ID")

	if botToken == "" || guildID == "" || verifiedRoleID == "" || gmailAddress == "" || gmailAppPassword == "" || welcomeChannelID == "" {
		log.Fatal("Error: Not all required environment variables are set. (DISCORD_PRIVATE_CATEGORY_ID is optional)")
	}
}

// --- Main Function ---
func main() {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(onReady)
	dg.AddHandler(interactionHandler)

	dg.Identify.Intents = discordgo.IntentsGuilds

	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}

	log.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	log.Println("Shutting down bot.")
	dg.Close()
}

// --- Handlers ---

func onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("Logged in as: %s#%s", s.State.User.Username, s.State.User.Discriminator)

	commands := []*discordgo.ApplicationCommand{
		{Name: "verify", Description: "Start verification with your Kosen email.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "email", Description: "Your Kosen email address", Required: true}}},
		{Name: "code", Description: "Enter the verification code sent to your email.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "code", Description: "The 6-digit verification code", Required: true}}},
	}

	log.Println("Registering commands...")
	_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, commands)
	if err != nil {
		log.Fatalf("Could not register commands: %v", err)
	}
	log.Println("Commands successfully registered.")

	setupVerificationButton(s)
}

func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		switch i.ApplicationCommandData().Name {
		case "verify":
			handleVerify(s, i)
		case "code":
			handleCode(s, i)
		}
	case discordgo.InteractionMessageComponent:
		if i.MessageComponentData().CustomID == startVerificationButtonID {
			handleStartVerification(s, i)
		}
	}
}

// --- Logic Functions ---

func handleVerify(s *discordgo.Session, i *discordgo.InteractionCreate) {
	email := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	domainParts := strings.Split(email, "@")
	if len(domainParts) != 2 || !isValidKosenEmail(domainParts[1]) {
		respondEphemeral(s, i, "Error: Please use a valid Kosen email address ending in `kosen-ac.jp`.")
		return
	}

	code, err := generateVerificationCode()
	if err != nil {
		log.Printf("Failed to generate code: %v", err)
		respondEphemeral(s, i, "Error: An internal issue occurred. Please contact an admin.")
		return
	}

	codesMutex.Lock()
	verificationCodes[userID] = code
	codesMutex.Unlock()

	err = sendVerificationEmail(email, code)
	if err != nil {
		log.Printf("Failed to send email: %v", err)
		respondEphemeral(s, i, "Error: Failed to send verification email. Please try again later.")
		return
	}

	respondEphemeral(s, i, "A 6-digit verification code has been sent to your email. Please use the `/code` command to complete verification.")
}

func handleCode(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userCode := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	codesMutex.Lock()
	correctCode, ok := verificationCodes[userID]
	codesMutex.Unlock()

	if !ok || userCode != correctCode {
		respondEphemeral(s, i, "Error: The verification code is incorrect.")
		return
	}

	err := s.GuildMemberRoleAdd(i.GuildID, userID, verifiedRoleID)
	if err != nil {
		log.Printf("Failed to add role: %v", err)
		respondEphemeral(s, i, "Error: Failed to assign role. Please contact an admin.")
		return
	}

	respondEphemeral(s, i, "Verification successful! This channel will be deleted in 10 seconds.")

	codesMutex.Lock()
	delete(verificationCodes, userID)
	codesMutex.Unlock()

	time.Sleep(10 * time.Second)
	_, err = s.ChannelDelete(i.ChannelID)
	if err != nil {
		log.Printf("Failed to delete channel: %v", err)
	}
}

func handleStartVerification(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Creating a private verification channel for you...",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})

	channelName := fmt.Sprintf("認証-%s", i.Member.User.Username)
	channel, err := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:     channelName,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: privateCategoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			{ID: i.Member.User.ID, Type: discordgo.PermissionOverwriteTypeMember, Allow: discordgo.PermissionViewChannel},
		},
	})
	if err != nil {
		log.Printf("Failed to create private channel: %v", err)
		return
	}

	embed := &discordgo.MessageEmbed{
		Title:       "Welcome! Let's start verification.",
		Description: "This is a private channel visible only to you and the bot.\nPlease follow the steps below to complete verification.",
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Step 1: Register Email", Value: "Use `/verify email:your-kosen-email@s.kosen-ac.jp`"},
			{Name: "Step 2: Enter Code", Value: "Use `/code code:123456` with the code you receive in your email."},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "This channel will be deleted automatically upon successful verification."},
		Color:  0x5865F2,
	}

	s.ChannelMessageSendEmbed(channel.ID, embed)
}

// --- Helper Functions ---

func setupVerificationButton(s *discordgo.Session) {
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "Tap Here to Start Verification",
				Style:    discordgo.PrimaryButton,
				CustomID: startVerificationButtonID,
				// FIX 1: Added '&' to provide a pointer to the struct, not the struct itself.
			
			},
		}},
	}

	embed := &discordgo.MessageEmbed{
		Title:       "Kosen Student Verification System",
		Description: "To access all server features, you must verify that you are a Kosen student.\nPress the button below to create a private channel and begin the process.",
		Color:       0x5865F2,
	}

	messages, err := s.ChannelMessages(welcomeChannelID, 10, "", "", "")
	if err != nil {
		log.Printf("Could not get channel messages: %v", err)
		return
	}

	var botMessage *discordgo.Message
	for _, msg := range messages {
		if msg.Author.ID == s.State.User.ID {
			botMessage = msg
			break
		}
	}

	if botMessage == nil {
		s.ChannelMessageSendComplex(welcomeChannelID, &discordgo.MessageSend{Embed: embed, Components: components})
	} else {
		// FIX 2: Removed extra '&' from 'embed' because it's already a pointer.
		s.ChannelMessageEditComplex(&discordgo.MessageEdit{Channel: welcomeChannelID, ID: botMessage.ID, Embed: embed, Components: &components})
	}
	log.Println("Verification button setup/update complete.")
}

func isValidKosenEmail(domain string) bool {
	return domain == "kosen-ac.jp" || strings.HasSuffix(domain, ".kosen-ac.jp")
}

func generateVerificationCode() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", int(b[0])<<24|int(b[1])<<16|int(b[2])<<8|int(b[3]))[:6], nil
}

func sendVerificationEmail(recipient, code string) error {
	auth := smtp.PlainAuth("", gmailAddress, gmailAppPassword, "smtp.gmail.com")
	msg := []byte("To: " + recipient + "\r\n" + "Subject: Discord Verification Code\r\n\r\n" + "Your Discord verification code is: " + code + "\r\n")
	return smtp.SendMail("smtp.gmail.com:587", auth, gmailAddress, []string{recipient}, msg)
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral},
	})
}
