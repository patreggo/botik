package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/examples"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

func main() {
	// Environment variables:
	//	BOT_TOKEN:     token from BotFather
	// 	APP_ID:        app_id of Telegram app.
	// 	APP_HASH:      app_hash of Telegram app.
	// 	SESSION_FILE:  path to session file
	// 	SESSION_DIR:   path to session directory, if SESSION_FILE is not set
	examples.Run(func(ctx context.Context, log *zap.Logger) error {
		// Dispatcher handles incoming updates.
		dispatcher := tg.NewUpdateDispatcher()
		opts := telegram.Options{
			Logger:        log,
			UpdateHandler: dispatcher,
		}
		return telegram.BotFromEnvironment(ctx, opts, func(ctx context.Context, client *telegram.Client) error {
			// Raw MTProto API client, allows making raw RPC calls.
			api := tg.NewClient(client)
			// Helper for sending messages.
			sender := message.NewSender(api)

			// Setting up handler for incoming message.
			dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage) error {
				m, ok := u.Message.(*tg.Message)
				if !ok || m.Out {
					// Outgoing message, not interesting.
					return nil
				}
				log.Info("Received message", zap.String("text", m.Message), zap.Any("peer", m.GetPeerID()))
				// Проверяем команды для тегания всех участников
				text := strings.TrimSpace(m.Message)
				if text == "/tagall" || text == "/all" || text == "@all" {
					return tagAllUsers(ctx, api, sender, entities, u, m)
				}

				return nil
			})
			return nil
		}, telegram.RunUntilCanceled)
	})
}

// tagAllUsers теги всех участников чата
func tagAllUsers(ctx context.Context, api *tg.Client, sender *message.Sender, entities tg.Entities, u *tg.UpdateNewMessage, m *tg.Message) error {
	// Получаем peer ID
	peerID := m.GetPeerID()

	// Проверяем тип чата и получаем участников
	switch peer := peerID.(type) {
	case *tg.PeerChat:
		// Обычная группа
		return tagUsersInChat(ctx, api, sender, entities, u, peer.ChatID)
	case *tg.PeerChannel:
		// Супергруппа или канал
		return tagUsersInSupergroup(ctx, api, sender, entities, u, peer.ChannelID)
	case *tg.PeerUser:
		// Личные сообщения - команда не работает
		_, err := sender.Reply(entities, u).Text(ctx, "Эта команда работает только в групповых чатах!")
		return err
	}

	return nil
}

// tagUsersInChat теги участников в обычной группе
func tagUsersInChat(ctx context.Context, api *tg.Client, sender *message.Sender, entities tg.Entities, u *tg.UpdateNewMessage, chatID int64) error {
	// Получаем полную информацию о чате
	fullChat, err := api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return err
	}

	chatFull, ok := fullChat.FullChat.(*tg.ChatFull)
	if !ok {
		return fmt.Errorf("unexpected chat type")
	}

	// Получаем участников
	participants, ok := chatFull.Participants.(*tg.ChatParticipants)
	if !ok {
		return fmt.Errorf("unexpected participants type")
	}

	// Собираем всех пользователей
	var mentions []string
	for _, participant := range participants.Participants {
		var userID int64

		switch p := participant.(type) {
		case *tg.ChatParticipant:
			userID = p.UserID
		case *tg.ChatParticipantAdmin:
			userID = p.UserID
		case *tg.ChatParticipantCreator:
			userID = p.UserID
		default:
			continue
		}

		// Ищем пользователя в entities
		if userEntity, ok := entities.Users[userID]; ok {
			if !userEntity.Bot && !userEntity.Deleted {
				username := getUserMention(userEntity)
				if username != "" {
					mentions = append(mentions, username)
				}
			}
		}
	}

	return sendMentions(ctx, sender, entities, u, mentions)
}

// tagUsersInSupergroup теги участников в супергруппе
func tagUsersInSupergroup(ctx context.Context, api *tg.Client, sender *message.Sender, entities tg.Entities, u *tg.UpdateNewMessage, channelID int64) error {
	// Находим канал в entities для получения access_hash
	var channel *tg.Channel
	for _, chatEntity := range entities.Chats {
		if ch, ok := chatEntity.AsNotEmpty(); ok {
			if channel, ok := ch.(*tg.Channel); ok && channel.ID == channelID {
				channel = ch.(*tg.Channel)
				break
			}
		}
	}

	if channel == nil {
		_, err := sender.Reply(entities, u).Text(ctx, "Не удалось получить информацию о чате.")
		return err
	}

	// Получаем участников супергруппы
	participants, err := api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: &tg.InputChannel{
			ChannelID:  channel.ID,
			AccessHash: channel.AccessHash,
		},
		Filter: &tg.ChannelParticipantsRecent{},
		Offset: 0,
		Limit:  200, // Максимум 200 участников за раз
		Hash:   0,
	})
	if err != nil {
		return err
	}

	channelParticipants, ok := participants.(*tg.ChannelsChannelParticipants)
	if !ok {
		return fmt.Errorf("unexpected participants type")
	}

	// Собираем всех пользователей
	var mentions []string
	for _, participant := range channelParticipants.Participants {
		var userID int64

		switch p := participant.(type) {
		case *tg.ChannelParticipant:
			userID = p.UserID
		case *tg.ChannelParticipantSelf:
			userID = p.UserID
		case *tg.ChannelParticipantAdmin:
			userID = p.UserID
		case *tg.ChannelParticipantCreator:
			userID = p.UserID
		default:
			continue
		}

		// Ищем пользователя в полученных участниках
		for _, u := range channelParticipants.Users {
			if usr, ok := u.(*tg.User); ok {
				if usr.ID == userID && !usr.Bot && !usr.Deleted {
					username := getUserMention(usr)
					if username != "" {
						mentions = append(mentions, username)
					}
					break
				}
			}
		}
	}

	return sendMentions(ctx, sender, entities, u, mentions)
}

// sendMentions отправляет сообщения с упоминаниями
func sendMentions(ctx context.Context, sender *message.Sender, entities tg.Entities, u *tg.UpdateNewMessage, mentions []string) error {
	if len(mentions) == 0 {
		_, err := sender.Reply(entities, u).Text(ctx, "Не найдено активных участников для упоминания.")
		return err
	}

	// Разбиваем на части если слишком много участников
	const maxMentionsPerMessage = 50
	const maxMessageLength = 4000 // Лимит длины сообщения в Telegram

	if len(mentions) <= maxMentionsPerMessage {
		// Если участников немного, отправляем одним сообщением
		text := "📢 Внимание всех участников:\n" + strings.Join(mentions, " ")
		if len(text) <= maxMessageLength {
			_, err := sender.Reply(entities, u).Text(ctx, text)
			return err
		}
	}

	// Отправляем по частям
	for i := 0; i < len(mentions); i += maxMentionsPerMessage {
		end := i + maxMentionsPerMessage
		if end > len(mentions) {
			end = len(mentions)
		}

		batch := mentions[i:end]
		text := fmt.Sprintf("📢 Внимание участников (%d-%d):\n%s",
			i+1, end, strings.Join(batch, " "))

		// Проверяем длину сообщения
		if len(text) > maxMessageLength {
			// Если даже часть слишком длинная, уменьшаем размер батча
			smallerBatch := batch[:len(batch)/2]
			text = fmt.Sprintf("📢 Внимание участников:\n%s",
				strings.Join(smallerBatch, " "))
			i -= maxMentionsPerMessage / 2 // Корректируем индекс
		}

		_, err := sender.Reply(entities, u).Text(ctx, text)
		if err != nil {
			return err
		}
	}

	return nil
}

// getUserMention возвращает упоминание пользователя
func getUserMention(user *tg.User) string {
	if user.Username != "" {
		return "@" + user.Username
	}

	// Если нет username, используем имя с ID для markdown-ссылки
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}

	// Экранируем специальные символы для markdown
	name = strings.ReplaceAll(name, "[", "\\[")
	name = strings.ReplaceAll(name, "]", "\\]")
	name = strings.ReplaceAll(name, "(", "\\(")
	name = strings.ReplaceAll(name, ")", "\\)")

	// Возвращаем текстовое упоминание с ID
	return fmt.Sprintf("[%s](tg://user?id=%d)", name, user.ID)
}
