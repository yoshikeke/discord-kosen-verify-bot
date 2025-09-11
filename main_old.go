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

// --- グローバル変数 ---

var (
	// Botの設定値 (環境変数から読み込む)
	botToken          string
	guildID           string // Botが動作するサーバー(Guild)のID
	verifiedRoleID    string // 付与するロールのID
	gmailAddress      string // 送信元Gmailアドレス
	gmailAppPassword  string // Gmailアプリパスワード
	welcomeChannelID  string // 認証開始ボタンを設置するチャンネルのID
	privateCategoryID string // プライベートチャンネルを作成するカテゴリのID (任意)

	// ユーザーIDと認証コードを紐づけて保存するマップ
	verificationCodes = make(map[string]string)
	codesMutex        = &sync.Mutex{}
)

// --- 定数 (新規追加) ---
const (
	// 認証開始ボタンに付けるユニークなID
	startVerificationButtonID = "start_verification_button"
)

// --- 初期化処理 ---
func init() {
	botToken = os.Getenv("DISCORD_BOT_TOKEN")
	guildID = os.Getenv("DISCORD_GUILD_ID")
	verifiedRoleID = os.Getenv("DISCORD_VERIFIED_ROLE_ID")
	gmailAddress = os.Getenv("GMAIL_ADDRESS")
	gmailAppPassword = os.Getenv("GMAIL_APP_PASSWORD")
	welcomeChannelID = os.Getenv("DISCORD_WELCOME_CHANNEL_ID")
	privateCategoryID = os.Getenv("DISCORD_PRIVATE_CATEGORY_ID") // 任意設定

	if botToken == "" || guildID == "" || verifiedRoleID == "" || gmailAddress == "" || gmailAppPassword == "" || welcomeChannelID == "" {
		log.Fatal("エラー: 必要な環境変数がすべて設定されていません。(DISCORD_PRIVATE_CATEGORY_IDは任意です)")
	}
}

// --- メイン処理 ---
func main() {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("Discordセッションの作成中にエラーが発生しました: %v", err)
	}

	dg.AddHandler(onReady)
	// スラッシュコマンドとボタンクリックの両方を処理するハンドラに変更
	dg.AddHandler(interactionHandler)

	dg.Identify.Intents = discordgo.IntentsGuilds

	err = dg.Open()
	if err != nil {
		log.Fatalf("WebSocket接続中にエラーが発生しました: %v", err)
	}

	log.Println("Botが起動しました。終了するには CTRL-C を押してください。")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	log.Println("Botをシャットダウンします。")
	dg.Close()
}

// --- イベントハンドラ ---

// Bot準備完了時のハンドラ (機能追加)
func onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("%s#%s としてログインしました", s.State.User.Username, s.State.User.Discriminator)

	// ... (スラッシュコマンド登録処理は変更なし)
	commands := []*discordgo.ApplicationCommand{
		{Name: "verify", Description: "高専のメールアドレスで認証を開始します。", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "email", Description: "高専のメールアドレス", Required: true}}},
		{Name: "code", Description: "メールで受信した認証コードを入力します。", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "code", Description: "6桁の認証コード", Required: true}}},
	}
	log.Println("コマンドを登録しています...")
	_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, commands)
	if err != nil {
		log.Fatalf("コマンドの登録に失敗しました: %v", err)
	}
	log.Println("コマンドの登録が完了しました。")

	// --- "認証開始"ボタンを設置する処理 (新規追加) ---
	setupVerificationButton(s)
}

// スラッシュコマンドとボタンクリックを処理するハンドラ (更新)
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
		// ボタンがクリックされた場合の処理
		if i.MessageComponentData().CustomID == startVerificationButtonID {
			handleStartVerification(s, i)
		}
	}
}

// --- コマンド処理 ---

// `/verify` コマンドの処理 (変更なし)
func handleVerify(s *discordgo.Session, i *discordgo.InteractionCreate) {
	email := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	domainParts := strings.Split(email, "@")
	if len(domainParts) != 2 || !isValidKosenEmail(domainParts[1]) {
		respondEphemeral(s, i, "エラー: `*@*.kosen-ac.jp` 形式のメールアドレスを入力してください。")
		return
	}

	code, err := generateVerificationCode()
	if err != nil {
		log.Printf("コードの生成に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: 内部で問題が発生しました。管理者に連絡してください。")
		return
	}

	codesMutex.Lock()
	verificationCodes[userID] = code
	codesMutex.Unlock()

	err = sendVerificationEmail(email, code)
	if err != nil {
		log.Printf("メールの送信に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: メールの送信に失敗しました。時間をおいて再度お試しください。")
		return
	}

	respondEphemeral(s, i, "あなたのメールアドレスに認証コードを送信しました。\n`/code` コマンドでコードを入力してください。")
}

// `/code` コマンドの処理 (機能追加: チャンネル削除)
func handleCode(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userCode := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	codesMutex.Lock()
	correctCode, ok := verificationCodes[userID]
	codesMutex.Unlock()

	if !ok || userCode != correctCode {
		respondEphemeral(s, i, "認証コードが正しくありません。")
		return
	}

	err := s.GuildMemberRoleAdd(i.GuildID, userID, verifiedRoleID)
	if err != nil {
		log.Printf("ロールの付与に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: ロールの付与に失敗しました。サーバー管理者に連絡してください。")
		return
	}

	respondEphemeral(s, i, "認証が完了しました！このチャンネルは10秒後に自動で削除されます。")

	// 使用

	// 使用済みのコードを削除
	codesMutex.Lock()
	delete(verificationCodes, userID)
	codesMutex.Unlock()
}

// --- ヘルパー関数 ---

// メールアドレスのドメインを検証する関数
func isValidKosenEmail(domain string) bool {
	return domain == "kosen-ac.jp" || strings.HasSuffix(domain, ".kosen-ac.jp")
}

// 6桁の認証コードを生成する関数
func generateVerificationCode() (string, error) {
	b := make([]byte, 4) // 넉넉하게 4바이트를 읽습니다.
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 6桁の数字コードを生成
	return fmt.Sprintf("%06d", int(b[0])<<24|int(b[1])<<16|int(b[2])<<8|int(b[3]))[:6], nil
}

// 認証メールを送信する関数
func sendVerificationEmail(recipient, code string) error {
	auth := smtp.PlainAuth("", gmailAddress, gmailAppPassword, "smtp.gmail.com")
	msg := []byte(
		"To: " + recipient + "\r\n" +
			"Subject: Discord 認証コード\r\n" +
			"\r\n" +
			"あなたのDiscord認証コードは: " + code + " です。\r\n",
	)
	return smtp.SendMail("smtp.gmail.com:587", auth, gmailAddress, []string{recipient}, msg)
}

// エフェメラルメッセージ（本人にしか見えないメッセージ）を送信する関数
func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
