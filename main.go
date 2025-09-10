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

	"github.com/bwmarrin/discordgo"
)

// --- グローバル変数 ---

var (
	// Botの設定値 (環境変数から読み込む)
	botToken         string
	guildID          string // Botが動作するサーバー(Guild)のID
	verifiedRoleID   string // 付与するロールのID
	gmailAddress     string // 送信元Gmailアドレス
	gmailAppPassword string // Gmailアプリパスワード

	// ユーザーIDと認証コードを紐づけて保存するマップ
	// [userID] -> "code"
	verificationCodes = make(map[string]string)
	// マップをスレッドセーフに操作するためのMutex
	codesMutex = &sync.Mutex{}
)

// --- 初期化処理 ---

// main関数より先に実行され、環境変数を読み込む
func init() {
	botToken = os.Getenv("DISCORD_BOT_TOKEN")
	guildID = os.Getenv("DISCORD_GUILD_ID")
	verifiedRoleID = os.Getenv("DISCORD_VERIFIED_ROLE_ID")
	gmailAddress = os.Getenv("GMAIL_ADDRESS")
	gmailAppPassword = os.Getenv("GMAIL_APP_PASSWORD")

	// 必須の環境変数が設定されているかチェック
	if botToken == "" || guildID == "" || verifiedRoleID == "" || gmailAddress == "" || gmailAppPassword == "" {
		log.Fatal("エラー: 必要な環境変数がすべて設定されていません。")
	}
}

// --- メイン処理 ---

func main() {
	// Discordセッションを作成
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("Discordセッションの作成中にエラーが発生しました: %v", err)
	}

	// Bot起動時に実行される処理 (スラッシュコマンドの登録)
	dg.AddHandler(onReady)

	// スラッシュコマンドが実行されたときに呼び出される処理
	dg.AddHandler(commandHandler)

	// Botがメッセージなどの情報を受け取れるようにIntentsを設定
	dg.Identify.Intents = discordgo.IntentsGuilds

	// WebSocket接続を開始
	err = dg.Open()
	if err != nil {
		log.Fatalf("WebSocket接続中にエラーが発生しました: %v", err)
	}

	// Botが終了するまで待機 (Ctrl+Cで終了)
	log.Println("Botが起動しました。終了するには CTRL-C を押してください。")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// セッションをクリーンに閉じる
	log.Println("Botをシャットダウンします。")
	dg.Close()
}

// --- イベントハンドラ ---

// Bot準備完了時のハンドラ
func onReady(s *discordgo.Session, r *discordgo.Ready) {
	log.Printf("%s#%s としてログインしました", s.State.User.Username, s.State.User.Discriminator)

	// スラッシュコマンドの定義
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "verify",
			Description: "高専のメールアドレスで認証を開始します。",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "email",
					Description: "高専のメールアドレス",
					Required:    true,
				},
			},
		},
		{
			Name:        "code",
			Description: "メールで受信した認証コードを入力します。",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "code",
					Description: "6桁の認証コード",
					Required:    true,
				},
			},
		},
	}

	// スラッシュコマンドをサーバーに登録
	log.Println("コマンドを登録しています...")
	_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, commands)
	if err != nil {
		log.Fatalf("コマンドの登録に失敗しました: %v", err)
	}
	log.Println("コマンドの登録が完了しました。")
}

// スラッシュコマンド実行時のハンドラ
func commandHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Interactionがスラッシュコマンドでなければ何もしない
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	switch i.ApplicationCommandData().Name {
	case "verify":
		handleVerify(s, i)
	case "code":
		handleCode(s, i)
	}
}

// --- コマンド処理 ---

// `/verify` コマンドの処理
func handleVerify(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// ユーザーが入力したメールアドレスを取得
	email := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	// メールドメインのバリデーション
	domainParts := strings.Split(email, "@")
	if len(domainParts) != 2 || !isValidKosenEmail(domainParts[1]) {
		respondEphemeral(s, i, "エラー: `*@*.kosen-ac.jp` 形式のメールアドレスを入力してください。")
		return
	}

	// 認証コードを生成
	code, err := generateVerificationCode()
	if err != nil {
		log.Printf("コードの生成に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: 内部で問題が発生しました。管理者に連絡してください。")
		return
	}

	// コードを保存 (Mutexで排他制御)
	codesMutex.Lock()
	verificationCodes[userID] = code
	codesMutex.Unlock()

	// メールを送信
	err = sendVerificationEmail(email, code)
	if err != nil {
		log.Printf("メールの送信に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: メールの送信に失敗しました。時間をおいて再度お試しください。")
		return
	}

	// ユーザーに応答
	respondEphemeral(s, i, "あなたのメールアドレスに認証コードを送信しました。\n`/code` コマンドでコードを入力してください。")
}

// `/code` コマンドの処理
func handleCode(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userCode := i.ApplicationCommandData().Options[0].StringValue()
	userID := i.Member.User.ID

	// 保存されたコードを取得
	codesMutex.Lock()
	correctCode, ok := verificationCodes[userID]
	codesMutex.Unlock()

	if !ok || userCode != correctCode {
		respondEphemeral(s, i, "認証コードが正しくありません。")
		return
	}

	// ロールを付与
	err := s.GuildMemberRoleAdd(i.GuildID, userID, verifiedRoleID)
	if err != nil {
		log.Printf("ロールの付与に失敗しました: %v", err)
		respondEphemeral(s, i, "エラー: ロールの付与に失敗しました。サーバー管理者に連絡してください。")
		return
	}

	// 成功メッセージを送信
	respondEphemeral(s, i, "認証が完了しました！ようこそ！")

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