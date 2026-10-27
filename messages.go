package main

import "github.com/kryspal/plugin-sdk/i18n"

// messages is the declared string table: en, zh-TW and zh-CN; other languages follow the i18n fallback chain, ending in English.
// The attribution (attribution.text) is the English original, the same in every language, and is not here.
var messages = i18n.New(messageTables)

var messageTables = map[string]map[string]string{
	"en": {
		"desc":             "Each member links their own Trakt account to report what they watch and sync watched history. Also adds the Trakt community rating to movies and series.",
		"groupAdvanced":    "Advanced",
		"idLabel":          "Trakt application client ID",
		"idHelp":           "Leave empty to use the built-in application. Fill it only to use an application you registered yourself on Trakt.",
		"idLink":           "Your Trakt applications",
		"secretLabel":      "Trakt application client secret",
		"secretHelp":       "Goes with your own client ID. Leave empty for an application that has no secret.",
		"baseURLLabel":     "API base URL",
		"baseURLHelp":      "Leave empty to use the official API. Only for a trusted HTTPS proxy.",
		"errSecretNeedsID": "Enter the client ID that goes with this secret.",
		"errNoApp":         "This build has no built-in Trakt application. Enter your own client ID.",
		"errHTTPS":         "Must be an https:// address.",
	},
	"zh-TW": {
		"desc":             "每位成員連結自己的 Trakt 帳號，回報正在觀看的內容並同步已看紀錄。另外為電影和劇集加上 Trakt 社群評分。",
		"groupAdvanced":    "進階",
		"idLabel":          "Trakt 應用程式 Client ID",
		"idHelp":           "留空表示使用內建的應用程式。只有要用你自己在 Trakt 註冊的應用程式時才填。",
		"idLink":           "你的 Trakt 應用程式",
		"secretLabel":      "Trakt 應用程式 Client Secret",
		"secretHelp":       "與你自己的 Client ID 搭配。沒有密鑰的應用程式請留空。",
		"baseURLLabel":     "API 網址",
		"baseURLHelp":      "留空表示使用官方 API。只用於可信任的 HTTPS 代理。",
		"errSecretNeedsID": "請填入這個密鑰對應的 Client ID。",
		"errNoApp":         "這個版本沒有內建的 Trakt 應用程式，請填入你自己的 Client ID。",
		"errHTTPS":         "必須是 https:// 網址。",
	},
	"zh-CN": {
		"desc":             "每位成员连接自己的 Trakt 账号，汇报正在观看的内容并同步已看记录。另外为电影和剧集加上 Trakt 社群评分。",
		"groupAdvanced":    "高级",
		"idLabel":          "Trakt 应用 Client ID",
		"idHelp":           "留空表示使用内置的应用。只有要用你自己在 Trakt 注册的应用时才填。",
		"idLink":           "你的 Trakt 应用",
		"secretLabel":      "Trakt 应用 Client Secret",
		"secretHelp":       "与你自己的 Client ID 搭配。没有密钥的应用请留空。",
		"baseURLLabel":     "API 地址",
		"baseURLHelp":      "留空表示使用官方 API。只用于可信任的 HTTPS 代理。",
		"errSecretNeedsID": "请填入这个密钥对应的 Client ID。",
		"errNoApp":         "这个版本没有内置的 Trakt 应用，请填入你自己的 Client ID。",
		"errHTTPS":         "必须是 https:// 地址。",
	},
}
