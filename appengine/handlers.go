package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fishy/url2epub"
	"github.com/fishy/url2epub/logger"
	"github.com/fishy/url2epub/rmapi"
	"github.com/fishy/url2epub/tgbot"
)

const (
	startErrMsg  = `🚫 Failed to register token %q. Please double check your token is correct. It should be a 8-digit code from https://my.remarkable.com/connect/desktop.`
	startSaveErr = `🚫 Failed to save this registration. Please try again later.`
	startExplain = `ℹ️ To link your reMarkable account, go to https://my.remarkable.com/connect/desktop, copy the 8-digit code, and come back to type "` + startCommand + ` <8-digit code>"`
	startSuccess = `✅ Successfully linked your reMarkable account! By default all epubs are sent to your root directory. To set a different one, use "` + dirCommand + `" command. (Note that if you have a lot of files stored ` + dirCommand + ` command could be very slow or unable to success)`

	notStartedMsg = `🚫 You did not run ` + startCommand + ` command yet.`

	stopMsg = `✅ Successfully deleted your reMarkable token.`

	dirErrMsg     = `🚫 Failed to list directories. Please try again later.`
	dirMsg        = `✅ You are currently saving to "%s", please choose a new directory to save to:`
	dirSaveErr    = `🚫 Failed to save this directory. Please try again later.`
	dirOldErr     = `🚫 Failed to save this directory. Please try ` + dirCommand + ` command again later.`
	dirSuccess    = `✅ Saved!`
	dirSuccessMsg = `✅ Your new directory "%s" is saved.`

	noURLmsg          = `🚫 No URL found in message.`
	unsupportedURLmsg = `⚠️ Unsupported URL: "%s"`
	failedEpubMsg     = `🚫 Failed to generate epub from URL: "%s"`
	failedUpload      = `🚫 Failed to upload epub to your reMarkable account for URL: "%s"`
	successUpload     = `✅ Uploaded "%s.epub" to your reMarkable account from URL: "%s"`
)

func urlHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, message *tgbot.Message, text string) {
	chat := GetChat(ctx, message.Chat.ID)
	if chat == nil {
		replyMessage(ctx, w, message, notStartedMsg, true, nil)
		return
	}
	var url string
	for _, entity := range message.Entities {
		switch entity.Type {
		case "url":
			runes := []rune(text)
			if int64(len(runes)) < entity.Offset+entity.Length {
				errorLog.Printf("Unable to process url entity, entity = %v, msg = %q", entity, text)
				continue
			}
			url = string(runes[entity.Offset : entity.Offset+entity.Length])
			break
		case "text_link":
			url = entity.URL
			break
		}
	}
	if url == "" {
		replyMessage(ctx, w, message, noURLmsg, true, nil)
		return
	}
	id, title, data, err := getEpub(ctx, url, r.Header.Get("user-agent"))
	if err != nil {
		errorLog.Printf("getEpub failed for %q: %v", url, err)
		if errors.Is(err, errUnsupportedURL) {
			replyMessage(ctx, w, message, fmt.Sprintf(unsupportedURLmsg, url), true, nil)
		} else {
			replyMessage(ctx, w, message, fmt.Sprintf(failedEpubMsg, url), true, nil)
		}
		return
	}
	client := &rmapi.Client{
		RefreshToken: chat.Token,
		Logger:       logger.StdLogger(infoLog),
	}
	start := time.Now()
	size := data.Len()
	defer func() {
		infoLog.Printf("Upload took %v, epub size = %d, err = %v", time.Since(start), size, err)
	}()
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	err = client.Upload(ctx, rmapi.UploadArgs{
		ID:       id,
		Title:    title,
		Data:     data,
		Type:     rmapi.FileTypeEpub,
		ParentID: chat.GetParentID(),
	})
	if err != nil {
		errorLog.Printf("Upload failed for %q: %v", url, err)
		replyMessage(ctx, w, message, fmt.Sprintf(failedUpload, url), true, nil)
		return
	}
	replyMessage(ctx, w, message, fmt.Sprintf(successUpload, title, url), true, nil)
	infoLog.Printf("Uploaded epub to reMarkable, epub file size = %d, id = %q, title = %q", size, id, title)
}

func startHandler(ctx context.Context, w http.ResponseWriter, message *tgbot.Message, text string) {
	token := strings.TrimPrefix(text, startCommand)
	token = strings.TrimSpace(token)
	if token == "" {
		replyMessage(ctx, w, message, startExplain, true, nil)
		return
	}
	client, err := rmapi.Register(ctx, rmapi.RegisterArgs{
		Token:       token,
		Description: rmDescription,
	})
	if err != nil {
		errorLog.Printf("Unable to register: %v", err)
		replyMessage(ctx, w, message, fmt.Sprintf(
			startErrMsg,
			token,
		), true, nil)
		return
	}
	chat := &EntityChatToken{
		Chat:  message.Chat.ID,
		Token: client.RefreshToken,
	}
	if err := chat.SaveDatastore(ctx); err != nil {
		errorLog.Printf("Unable to save chat: %v", err)
		replyMessage(ctx, w, message, startSaveErr, true, nil)
		return
	}
	replyMessage(ctx, w, message, startSuccess, true, nil)
}

func stopHandler(ctx context.Context, w http.ResponseWriter, message *tgbot.Message) {
	chat := GetChat(ctx, message.Chat.ID)
	if chat == nil {
		replyMessage(ctx, w, message, notStartedMsg, true, nil)
		return
	}
	chat.Delete(ctx)
	replyMessage(ctx, w, message, stopMsg, true, nil)
}

func dirHandler(ctx context.Context, w http.ResponseWriter, message *tgbot.Message) {
	chat := GetChat(ctx, message.Chat.ID)
	if chat == nil {
		replyMessage(ctx, w, message, notStartedMsg, true, nil)
		return
	}
	client := &rmapi.Client{
		RefreshToken: chat.Token,
		Logger:       logger.StdLogger(infoLog),
	}
	dirs, err := client.ListDirs(ctx)
	if err != nil {
		errorLog.Printf("ListDirs failed: %v", err)
		replyMessage(ctx, w, message, dirErrMsg, true, nil)
		return
	}
	choices := make([][]tgbot.InlineKeyboardButton, 0, len(dirs))
	for id, name := range dirs {
		choices = append(choices, []tgbot.InlineKeyboardButton{
			{
				Text: name,
				Data: dirIDPrefix + id,
			},
		})
	}
	replyMessage(
		ctx,
		w,
		message,
		fmt.Sprintf(dirMsg, dirs[chat.GetParentID()]),
		true,
		&tgbot.InlineKeyboardMarkup{
			InlineKeyboard: choices,
		},
	)
}

func dirCallbackHandler(ctx context.Context, w http.ResponseWriter, data string, callback *tgbot.CallbackQuery) {
	if callback.Message == nil {
		errorLog.Printf("Bad callback, data = %q, callback = %#v", data, callback)
		getBot().ReplyCallback(ctx, callback.ID, dirOldErr)
		reply200(w)
		return
	}
	chat := GetChat(ctx, callback.Message.Chat.ID)
	if chat == nil {
		errorLog.Printf("Bad callback, data = %q, chat = %d", data, callback.Message.Chat.ID)
		getBot().ReplyCallback(ctx, callback.ID, notStartedMsg)
		reply200(w)
		return
	}
	chat.ParentID = data
	if err := chat.SaveDatastore(ctx); err != nil {
		errorLog.Printf("Unable to save chat: %v", err)
		getBot().ReplyCallback(ctx, callback.ID, dirSaveErr)
		reply200(w)
		return
	}
	if _, err := getBot().ReplyCallback(ctx, callback.ID, dirSuccess); err != nil {
		errorLog.Printf("Unable to reply callback: %v", err)
	}
	reply200(w)

	client := &rmapi.Client{
		RefreshToken: chat.Token,
		Logger:       logger.StdLogger(infoLog),
	}
	dirs, err := client.ListDirs(ctx)
	if err != nil {
		errorLog.Printf("Unable to list dir: %v", err)
		return
	}
	getBot().SendMessage(
		ctx,
		callback.Message.Chat.ID,
		fmt.Sprintf(dirSuccessMsg, dirs[chat.GetParentID()]),
		&callback.Message.ID,
		nil,
	)
}

func fontHandler(ctx context.Context, w http.ResponseWriter, message *tgbot.Message) {
	// TODO
}

func fontCallbackHandler(ctx context.Context, w http.ResponseWriter, data string, callback *tgbot.CallbackQuery) {
	// TODO
}

func reply200(w http.ResponseWriter) {
	code := http.StatusOK
	http.Error(w, http.StatusText(code), code)
}

func replyMessage(
	ctx context.Context,
	w http.ResponseWriter,
	orig *tgbot.Message,
	msg string,
	quote bool,
	markup *tgbot.InlineKeyboardMarkup,
) {
	reply := tgbot.ReplyMessage{
		Method:      "sendMessage",
		ChatID:      orig.Chat.ID,
		Text:        msg,
		ReplyMarkup: markup,
	}
	if quote {
		reply.ReplyTo = orig.ID
	}
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}

func replyCallback(
	ctx context.Context,
	w http.ResponseWriter,
	orig *tgbot.CallbackQuery,
	msg string,
) {
	reply := tgbot.AnswerCallbackQuery{
		ID:   orig.ID,
		Text: msg,
	}
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}

var errUnsupportedURL = errors.New("unsupported URL")

func getEpub(ctx context.Context, url string, ua string) (id, title string, data *bytes.Buffer, err error) {
	ctx, cancel := context.WithTimeout(ctx, epubTimeout)
	defer cancel()
	root, baseURL, err := url2epub.GetHTML(ctx, url2epub.GetHTMLArgs{
		URL:       url,
		UserAgent: ua,
	})
	if err != nil {
		return "", "", nil, err
	}
	if !root.IsAMP() {
		ampURL := root.GetAMPurl()
		if ampURL == "" {
			return "", "", nil, errUnsupportedURL
		}
		root, baseURL, err = url2epub.GetHTML(ctx, url2epub.GetHTMLArgs{
			URL:       ampURL,
			UserAgent: ua,
		})
		if err != nil {
			return "", "", nil, err
		}
		if !root.IsAMP() {
			return "", "", nil, errUnsupportedURL
		}
	}
	node, images, err := root.Readable(ctx, url2epub.ReadableArgs{
		BaseURL:   baseURL,
		ImagesDir: "images",
	})
	if err != nil {
		return "", "", nil, err
	}
	if node == nil {
		// Should not happen
		return "", "", nil, errUnsupportedURL
	}

	buf := new(bytes.Buffer)
	data = buf
	title = root.GetTitle()
	id, err = url2epub.Epub(url2epub.EpubArgs{
		Dest:   buf,
		Title:  title,
		Node:   node,
		Images: images,
	})
	return
}
