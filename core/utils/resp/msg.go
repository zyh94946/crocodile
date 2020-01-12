package resp

import "errors"

var msgcode = map[int]string{
	Success: "ok",

	ErrUnauthorized: "非法请求",
	ErrBadRequest:   "请求参数错误",

	ErrUserPassword:  "请求参数错误",
	ErrUserForbid:    "禁止登陆",
	ErrEmailExist:    "邮箱已经存在",
	ErrUserNameExist: "用户名已存在",
	ErrUserNotExist:  "用户不存在",

	ErrTaskExist:    "任务名已存在",
	ErrTaskNotExist: "任务不存在",

	ErrHostgroupExist:    "主机组已存在",
	ErrHostgroupNotExist: "主机组不存在",
	ErrHostNotDeleteNeedDown: "worker不能删除，请先将worker下线",


	ErrInternalServer: "服务端错误",

	ErrCtxDeadlineExceeded: "调用超时",
	ErrCtxCanceled:         "取消调用",
	ErrRPCUnauthenticated:  "密钥认证失败",
	ErrRPCUnavailable:      "调用对端不可用",
	ErrRPCUnknow:           "调用未知错误",
	ErrRPCNotValidHost:     "未发现worker",
	ErrRPCNotConnHost:      "未找到存活的worker",
}

// GetMsg get msg by code
func GetMsg(code int) string {
	var (
		msg    string
		exists bool
	)

	if msg, exists = msgcode[code]; exists {
		return msg
	}
	return "unknown"
}

// GetMsgErr get error msg by code
func GetMsgErr(code int) error {
	msg := GetMsg(code)
	return errors.New(msg)
}