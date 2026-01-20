package liquidator

import "github.com/CSWellesSun/hypermevlib/utils"

func (ls *LiquidatorSystem) NotifyInfo(msg string) {
	go utils.NotifyTelegram(ls.TgBotToken, ls.TgChatIdForInfo, msg)
}
