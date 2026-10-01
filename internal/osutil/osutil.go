// Package osutil はOS依存の処理(ファイル選択ダイアログ・ブラウザ起動・エクスプローラー表示)。
package osutil

import "errors"

// ErrUnsupported はこのOSでは使えない機能。UI側はパスの手入力にフォールバックする。
var ErrUnsupported = errors.New("この環境ではダイアログを表示できません。パスを直接入力してください")

// DialogKind はダイアログの種類。
type DialogKind string

const (
	OpenExcel  DialogKind = "excel"
	OpenDB     DialogKind = "db"
	SaveDB     DialogKind = "savedb"
	PickFolder DialogKind = "folder"
)
