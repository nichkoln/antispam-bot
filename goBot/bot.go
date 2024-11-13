package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"os"
	"strconv"

	tgBotApi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

var trainingMode bool = false

var gracefulMode bool = false

var hour int = 2

// Структура для запроса на API
type TextItem struct {
	Text string `json:"text"`
}

// Структура для ответа от API
type APIResponse struct {
	Result string `json:"result"`
}

func initBot() (int64, *tgBotApi.BotAPI, tgBotApi.UpdatesChannel, map[int64]bool) {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	adminChatIDStr := os.Getenv("ADMIN_CHAT_ID")
	allowedChatsStr := os.Getenv("ALLOWED_CHATS")

	if botToken == "" {
		log.Panic("TELEGRAM_BOT_TOKEN is not set")
	}
	if adminChatIDStr == "" {
		log.Panic("ADMIN_CHAT_ID is not set")
	}
	if allowedChatsStr == "" {
		log.Panic("ALLOWED_CHATS is not set")
	}

	allowedChatsArr := strings.Split(allowedChatsStr, ";")
	allowedChatsArr = append(allowedChatsArr, adminChatIDStr)

	allowedChatsMap := make(map[int64]bool)

	for _, val := range allowedChatsArr {
		s, err := strconv.Atoi(val)
		if err != nil {
			log.Panic(err)
		}
		allowedChatsMap[int64(s)] = true
	}

	adminChatID, err := strconv.ParseInt(adminChatIDStr, 10, 64)
	if err != nil {
		log.Panic("ADMIN_CHAT_ID is not a valid int64")
	}

	bot, err := tgBotApi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true

	log.Printf("Авторизован как %s", bot.Self.UserName)
	log.Printf("Время карантина: %v час(ов)", hour)
	log.Printf("Тренировочный режим %v", trainingMode)

	u := tgBotApi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	return adminChatID, bot, updates, allowedChatsMap
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "/app/DB/usersDB.db")
	if err != nil {
		log.Fatal(err)
	}

	// Создание таблицы для хранения времени присоединения пользователей
	query := `
	CREATE TABLE IF NOT EXISTS user_joins (
		user_id INTEGER,
		chat_id INTEGER,
		join_time TIMESTAMP,
		PRIMARY KEY (user_id, chat_id)
	)`
	_, err = db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}
}

// Запись времени присоединения пользователя в базу данных
func saveJoinTime(userID int, chatID int64, joinTime time.Time) error {
	query := `
	INSERT OR REPLACE INTO user_joins (user_id, chat_id, join_time)
	VALUES (?, ?, ?)`
	_, err := db.Exec(query, userID, chatID, joinTime)
	return err
}

// Получение времени присоединения пользователя
func getJoinTime(userID int, chatID int64) (time.Time, error) {
	var joinTime time.Time
	query := `SELECT join_time FROM user_joins WHERE user_id = ? AND chat_id = ?`
	err := db.QueryRow(query, userID, chatID).Scan(&joinTime)
	if err != nil {
		return time.Time{}, err
	}
	return joinTime, nil
}

// Проверка наличия пользователя в базе данных
func isUserInDB(userID int, chatID int64) (bool, error) {
	var count int
	query := `SELECT COUNT(*) FROM user_joins WHERE user_id = ? AND chat_id = ?`
	err := db.QueryRow(query, userID, chatID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// Проверка времени вступления
func isNewUser(userID int, chatID int64) (bool, error) {
	joinTime, err := getJoinTime(userID, chatID)
	if err != nil {
		return false, err
	}

	// Проверяем, прошло ли 1 часов с момента присоединения
	if time.Since(joinTime) < time.Duration(hour)*time.Hour {
		return true, nil
	}
	return false, nil
}

// Функция для отправки текста на Python API для проверки спама
func checkSpam(text string) (bool, error) {
	apiURL := "http://pyrobertaapi:8001/predict/"

	// Создаем JSON-запрос
	textItem := TextItem{Text: text}
	jsonData, err := json.Marshal(textItem)
	if err != nil {
		return false, err
	}

	// Отправляем POST-запрос на Python API
	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// Читаем ответ от API
	var result APIResponse
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return false, err
	}

	// Проверяем результат
	if result.Result == "Спам" {
		return true, nil
	}
	return false, nil
}

// Функция для сохранения ложноположительного сообщения на Python API
func saveFalsePositive(text string) error {
	apiURL := "http://pyrobertaapi:8001/save_false_positive/"

	textItem := TextItem{Text: text}
	jsonData, err := json.Marshal(textItem)
	if err != nil {
		return err
	}

	_, err = http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	return err
}

// Функция для сохранения ложноотрицательного сообщения на Python API
func saveSpam(text string) error {
	apiURL := "http://pyrobertaapi:8001/save_spam/"

	textItem := TextItem{Text: text}
	jsonData, err := json.Marshal(textItem)
	if err != nil {
		return err
	}

	_, err = http.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	return err
}

// Функция для запуска переобучения модели на Python API
func retrainModel() error {
	apiURL := "http://pyrobertaapi:8001/retrain/"

	_, err := http.Post(apiURL, "application/json", nil)
	return err
}

// Функция для отправки сообщений с обработкой ошибки 429 (Too Many Requests)
func sendMessageWithRetry(bot *tgBotApi.BotAPI, msg tgBotApi.MessageConfig) error {
	// Попытка отправки сообщения с обработкой ошибок
	for {
		_, err := bot.Send(msg)
		if err == nil {
			// Сообщение отправлено успешно
			return nil
		}

		// Проверяем, является ли ошибка 429
		if apiErr, ok := err.(*tgBotApi.Error); ok {
			if apiErr.ResponseParameters.RetryAfter > 0 {
				// Если RetryAfter больше 0, ставим бота на паузу
				retryAfter := apiErr.ResponseParameters.RetryAfter
				log.Printf("Получена ошибка 429. Повторная попытка через %d секунд", retryAfter)
				time.Sleep(time.Duration(retryAfter) * time.Second)
				continue
			}
		}

		// Если ошибка не 429, выводим её и выходим
		return err
	}
}

// Асинхронная функция для отправки исчезающего уведомления
func sendTemporaryNotification(bot *tgBotApi.BotAPI, msg tgBotApi.MessageConfig) {
	// Используем go - routine для асинхронного выполнения
	go func() {
		// Отправляем уведомление
		notificationMessage, err := bot.Send(msg)
		if err != nil {
			log.Printf("Ошибка при отправке уведомления: %v", err)
			return
		}

		// Ожидаем 30 секунд перед удалением уведомления
		time.Sleep(30 * time.Second)

		// Удаляем уведомление
		deleteConfig := tgBotApi.NewDeleteMessage(notificationMessage.Chat.ID, notificationMessage.MessageID)
		_, err = bot.Request(deleteConfig)
		if err != nil {
			log.Printf("Ошибка при удалении уведомления: %v", err)
		}
	}()
}

func main() {
	initDB()

	adminChatID, bot, updates, allowedChatsMap := initBot()

	for update := range updates {

		//time.Sleep(100 * time.Millisecond)
		// Обработка текстовых сообщений и команд
		if update.Message != nil {
			// Проверка на нужный чат
			if _, inMap := allowedChatsMap[update.Message.Chat.ID]; !inMap {
				log.Printf("Неизвестный чат или пользователь%v", update.Message)
				continue
			}
			message := update.Message.Text
			userID := update.Message.From.ID
			chatID := update.Message.Chat.ID

			if message == "" && update.Message.Caption != "" {
				message = update.Message.Caption
			}

			chatMember, err := bot.GetChatMember(tgBotApi.GetChatMemberConfig{ChatConfigWithUser: tgBotApi.ChatConfigWithUser{
				ChatID: update.Message.Chat.ID,
				UserID: update.Message.From.ID,
			}})
			if err != nil {
				log.Printf("Не удалось получить информацию о пользователе: %v", err)
				continue
			}
			// Обработка команд
			if update.Message.IsCommand() && (chatMember.Status == "administrator" || chatMember.Status == "creator") {
				log.Printf("Получена команда %v", message)
				if message == "/trainingmode" {

					switch trainingMode {
					case true:
						trainingMode = false
						log.Println("Тренировка ВЫКЛ")
						adminMsg := tgBotApi.NewMessage(adminChatID, "Тренировка выкл")
						sendMessageWithRetry(bot, adminMsg)
					case false:
						trainingMode = true
						log.Println("Тренировка ВКЛ")
						adminMsg := tgBotApi.NewMessage(adminChatID, "Тренировка вкл")
						sendMessageWithRetry(bot, adminMsg)
					}
					continue
				} else if message[:8] == "/settime" {

					message = message[8:]

					hour, err = strconv.Atoi(strings.ReplaceAll(message, " ", ""))
					if err != nil {
						log.Printf("Ошибка конвертации времени: %v", err)
						continue
					}
					adminMsg := tgBotApi.NewMessage(adminChatID, fmt.Sprintf("Установлено время карантина %v часа(-ов)", hour))
					sendMessageWithRetry(bot, adminMsg)
					log.Printf("Установлено время карантина %v часа(-ов)", err)
					continue
				} else if message == "/retrain" && update.Message.Chat.ID == adminChatID {
					err := retrainModel()
					if err != nil {
						adminMsg := tgBotApi.NewMessage(adminChatID, "Ошибка при переобучении модели")
						sendMessageWithRetry(bot, adminMsg)
						log.Printf("Ошибка при переобучении: %v", err)
					} else {
						adminMsg := tgBotApi.NewMessage(adminChatID, "Модель успешно переобучена")
						sendMessageWithRetry(bot, adminMsg)
						log.Printf("Модель успешно переобучена")
					}
					continue
				} else if message == "/gracefulmode" {

					switch gracefulMode {
					case true:
						gracefulMode = false
						log.Println("Мирный ВЫКЛ")
						adminMsg := tgBotApi.NewMessage(adminChatID, "Мирный выкл")
						sendMessageWithRetry(bot, adminMsg)
					case false:
						gracefulMode = true
						log.Println("Мирный ВКЛ")
						adminMsg := tgBotApi.NewMessage(adminChatID, "Мирный вкл")
						sendMessageWithRetry(bot, adminMsg)
					}
					continue
				}

				// Обработка пользовательских сообщений с проверкой на спам
			} else {

				// Проверка наличия в бд времени вступления пользователя
				inDB, err := isUserInDB(int(userID), chatID)
				if err != nil {
					log.Printf("Не удалось получить информацию из БД: %v", err)
					continue
				}
				if !inDB {
					saveJoinTime(int(userID), chatID, time.Now())
				}
				// Проверяем, является ли пользователь администратором или владельцем и пропускаем проверку
				if (chatMember.Status == "administrator" || chatMember.Status == "creator") && !trainingMode {
					log.Printf("Пользователь %s является администратором, проверка на спам не выполняется", update.Message.From.UserName)
					continue // Пропускаем проверку на спам
				}

				//  Проверяем пользователя на предмет членства в группе < hours
				isNew, err := isNewUser(int(userID), chatID)
				if err != nil {
					log.Printf("Ошибка при проверке пользователя: %v", err)
					continue
				}

				// Проверка только для новых пользователей или режим тренировки
				if isNew || trainingMode {

					// Обычная проверка на спам
					isSpamMessage, err := checkSpam(message)
					if err != nil {
						log.Printf("Ошибка при проверке спама: %v", err)
						continue
					}

					// Если это спам, удаляем сообщение и уведомляем в админ чате
					if isSpamMessage {
						// Обычный режим
						if !gracefulMode {
							deleteMsg := tgBotApi.NewDeleteMessage(update.Message.Chat.ID, update.Message.MessageID)
							_, err := bot.Request(deleteMsg)
							if err != nil {
								log.Printf("Не удалось удалить сообщение: %v", err)
								adminMsg := tgBotApi.NewMessage(adminChatID, "Сообщение ниже является спамом, но не удалено из-за недостаточных прав")
								sendMessageWithRetry(bot, adminMsg)
								// Отправляем сообщение в админ чат
								adminMsg = tgBotApi.NewMessage(adminChatID, fmt.Sprint(message))

								// Создаем inline-кнопку "Не спам"
								callbackData := "fine"
								button := tgBotApi.NewInlineKeyboardButtonData("Не спам", callbackData)
								keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
								adminMsg.ReplyMarkup = keyboard

								// Отправляем сообщение в админ чат
								sendMessageWithRetry(bot, adminMsg)
							} else {
								log.Printf("Удалено спам-сообщение: %s", message)
								adminMsg := tgBotApi.NewMessage(adminChatID, "Сообщение ниже удалено как спам")
								sendMessageWithRetry(bot, adminMsg)
								// Отправляем сообщение в админ чат
								adminMsg = tgBotApi.NewMessage(adminChatID, fmt.Sprint(message))

								// Создаем inline-кнопку "Не спам"
								callbackData := "fine"
								button := tgBotApi.NewInlineKeyboardButtonData("Не спам", callbackData)
								keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
								adminMsg.ReplyMarkup = keyboard

								// Отправляем сообщение в админ чат
								sendMessageWithRetry(bot, adminMsg)
								tempMSG := tgBotApi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("@%v, Ваше сообщение было удалено модератором, поскольку наши фильтры распознали его, как спам. Попробуйте позднее или перефразируйте.", update.Message.From.UserName))
								sendTemporaryNotification(bot, tempMSG)
							}
							// Мирный режим
						} else {
							adminMsg := tgBotApi.NewMessage(adminChatID, "Сообщение ниже является спамом, но не удалено из-за мирного режима")
							sendMessageWithRetry(bot, adminMsg)
							// Отправляем сообщение в админ чат
							adminMsg = tgBotApi.NewMessage(adminChatID, fmt.Sprint(message))

							// Создаем inline-кнопку "Не спам"
							callbackData := "fine"
							button := tgBotApi.NewInlineKeyboardButtonData("Не спам", callbackData)
							keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
							adminMsg.ReplyMarkup = keyboard

							// Отправляем сообщение в админ чат
							sendMessageWithRetry(bot, adminMsg)
						}
						// Не спам
					} else {
						log.Printf("Не спам: %s", message)
						adminMsg := tgBotApi.NewMessage(adminChatID, "Сообщение ниже не является спамом")
						sendMessageWithRetry(bot, adminMsg)
						// Отправляем сообщение в админ чат
						adminMsg = tgBotApi.NewMessage(adminChatID, fmt.Sprint(message))

						// Создаем inline-кнопку "спам"
						callbackData := fmt.Sprintf("%s:%d:%d", "spam", update.Message.Chat.ID, update.Message.MessageID)
						button := tgBotApi.NewInlineKeyboardButtonData("Спам", callbackData)
						keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
						adminMsg.ReplyMarkup = keyboard

						// Отправляем сообщение в админ чат
						sendMessageWithRetry(bot, adminMsg)
					}
				} else {
					log.Printf("Пользователь  %v не является новым, проверка не выполняется", update.Message.From.UserName)
				}
			}

			// Обработка callback

		} else if update.CallbackQuery != nil {
			// Проверка на нужный чат
			if _, inMap := allowedChatsMap[update.CallbackQuery.Message.Chat.ID]; !inMap {
				log.Printf("Неизвестный чат или пользователь%v", update.Message)
				continue
			}
			log.Printf("Получено нажатие на кнопку %v", update.CallbackQuery.Data)
			// Обработка нажатия на кнопку "Не спам"
			if update.CallbackQuery.Data[:4] == "fine" {

				originalMessage := update.CallbackQuery.Message.Text
				// Сохраняем ложноположительное сообщение
				err := saveFalsePositive(originalMessage)
				if err != nil {
					log.Printf("Ошибка при сохранении ложноположительного сообщения: %v", err)
				}

				adminResponse := tgBotApi.NewMessage(adminChatID, "Сообщение было помечено как ложноположительное.")

				// Создаем кнопку для переобучения модели
				button := tgBotApi.NewInlineKeyboardButtonData("Переобучить модель", "retrain")
				keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
				adminResponse.ReplyMarkup = keyboard

				sendMessageWithRetry(bot, adminResponse)

				// Обработка кнопки retrain
			} else if update.CallbackQuery.Data == "retrain" {

				err := retrainModel()
				if err != nil {
					bot.Send(tgBotApi.NewMessage(adminChatID, "Ошибка при переобучении модели"))
					log.Printf("Ошибка при переобучении: %v", err)
				} else {
					bot.Send(tgBotApi.NewMessage(adminChatID, "Модель успешно переобучена"))
					log.Printf("Модель успешно переобучена")
				}

				// обработка ложноотрицательного срабатывания
			} else if update.CallbackQuery.Data[:4] == "spam" {

				callbackData := update.CallbackQuery.Data
				parts := strings.Split(callbackData, ":")
				originalMessage := update.CallbackQuery.Message.Text
				messageIDStr := parts[2]
				chatIDstr := parts[1]
				messageID, err := strconv.Atoi(messageIDStr)
				if err != nil {
					log.Printf("Ошибка при конвертации MessageID: %v", err)
					continue
				}
				chatID, err := strconv.Atoi(chatIDstr)
				if err != nil {
					log.Printf("Ошибка при конвертации chatID: %v", err)
					continue
				}

				// Удаление сообщения
				deleteMsg := tgBotApi.NewDeleteMessage(int64(chatID), messageID)
				_, err = bot.Request(deleteMsg)
				if err != nil {
					log.Printf("Не удалось удалить сообщение: %v", err)
				} else {
					log.Printf("Сообщение  %s удалено", originalMessage)
				}

				// Сохраняем текст спам сообщения
				err = saveSpam(originalMessage)
				if err != nil {
					log.Printf("Ошибка при сохранении spam: %v", err)
				}

				adminResponse := tgBotApi.NewMessage(adminChatID, "Сообщение было помечено как СПАМ. Для применения изменений требуется переобучить модель")

				// Создаем кнопку для переобучения модели
				button := tgBotApi.NewInlineKeyboardButtonData("Переобучить модель", "retrain")
				keyboard := tgBotApi.NewInlineKeyboardMarkup(tgBotApi.NewInlineKeyboardRow(button))
				adminResponse.ReplyMarkup = keyboard
				sendMessageWithRetry(bot, adminResponse)
			}
		}

	}
}
