package resp

const (
	// Success Success
	Success = 0
	// ErrBadRequest 非法请求
	ErrBadRequest   = 10400
	// ErrUnauthorized 非法请求
	ErrUnauthorized = 10401
	// ErrUserPassword 请求参数错误
	ErrUserPassword  = 10402
	// ErrUserForbid 禁止登陆
	ErrUserForbid    = 10403
	// ErrUserNameExist 邮箱已经存在
	ErrUserNameExist = 10413
	// ErrEmailExist 用户名已存在
	ErrEmailExist    = 10414
	// ErrUserNotExist 用户不存在
	ErrUserNotExist  = 10415

	// ErrTaskExist 任务名已存在
	ErrTaskExist    = 10416
	// ErrTaskNotExist 任务不存在
	ErrTaskNotExist = 10417

	// ErrHostgroupExist 主机组已存在
	ErrHostgroupExist    = 10418
	// ErrHostgroupNotExist 主机组不存在
	ErrHostgroupNotExist = 10419
	// ErrHostNotDeleteNeedDown 请先将worker下线后再删除
	ErrHostNotDeleteNeedDown = 10429

	// ErrInternalServer 服务端错误
	ErrInternalServer = 10500
	// ErrCtxDeadlineExceeded 调用超时
	ErrCtxDeadlineExceeded = 10600
	// ErrCtxCanceled 取消调用
	ErrCtxCanceled         = 10601

	// ErrRPCUnauthenticated  密钥认证失败
	ErrRPCUnauthenticated  = 10602
	// ErrRPCUnavailable 调用对端不可用
	ErrRPCUnavailable      = 10603
	// ErrRPCUnknow 调用未知错误
	ErrRPCUnknow           = 10604
	// ErrRPCNotValidHost  未发现worker
	ErrRPCNotValidHost     = 10605
	// ErrRPCNotConnHost 未找到存活的worker
	ErrRPCNotConnHost      = 10606
)