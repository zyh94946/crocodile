package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"errors"

	"github.com/go-redis/redis"
	"github.com/gorhill/cronexpr"
	"github.com/labulaka521/crocodile/common/errgroup"
	"github.com/labulaka521/crocodile/common/log"
	"github.com/labulaka521/crocodile/common/utils"
	"github.com/labulaka521/crocodile/core/alarm"
	"github.com/labulaka521/crocodile/core/config"
	"github.com/labulaka521/crocodile/core/model"
	pb "github.com/labulaka521/crocodile/core/proto"
	"github.com/labulaka521/crocodile/core/tasktype"
	"github.com/labulaka521/crocodile/core/utils/define"
	"github.com/labulaka521/crocodile/core/utils/resp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

var (
	// Cron2 schedule loop
	Cron2 *cacheSchedule2
)

var (
	// ErrNoGetLog get real log from redis where no data
	ErrNoGetLog = errors.New("no read data from cache")
)

// task running status
// redis key name:
type task2 struct {
	id        string             // taskid
	name      string             // taskname
	cronexpr  string             // cronexpr
	cronsub   time.Duration      // cronexpt sub
	close     chan struct{}      // stop schedule
	ctxcancel context.CancelFunc // store cancelfunc could cancel all task by this cancel
	next      Next               // it save a func Next by route policy
	canrun    bool               // task status

	sync.RWMutex               // lock
	redis        *redis.Client // redis client
	once         sync.Once     //

	errTaskID   string              // run fail task's id
	errTask     string              // run fail task's id
	errCode     int                 // failed task return code
	errMsg      string              // task run failed errmsg
	errTasktype define.TaskRespType // failed task type
}

const (
	// task
	taskstatus      string = "status"
	taskresp        string = "resp"
	taskrealtasklog string = "reallog"
)

func (t *task2) getdata(taskruntype define.TaskRespType, realid string, setdata string) (interface{}, error) {
	keyname := fmt.Sprintf("task:%s:%d:%s:%s", t.id, taskruntype, realid, setdata)

	switch setdata {
	case taskstatus:
		// 任务状态
		status, err := t.redis.Get(keyname).Int()
		if err != nil {
			return nil, err
		}
		return define.TaskStatus(status), nil
	case taskresp:
		// 任务数据
		res, err := t.redis.Get(keyname).Bytes()
		var tmptaskresp define.TaskResp
		err = json.Unmarshal(res, &tmptaskresp)
		if err != nil {
			return nil, err
		}
		return tmptaskresp, nil
	case taskrealtasklog:
		// 获取任务的全部日志
		var res []string
		err := t.redis.LRange(keyname, 0, -1).ScanSlice(&res)
		if err != nil {
			return nil, err
		}

		return strings.Join(res, ""), nil
	default:
		return nil, errors.New("unknow setdata")
	}

}

func (t *task2) SetData(tasrunktype define.TaskRespType, realid string,
	value interface{}, setdata string) error {

	return t.setdata(tasrunktype, realid, value, setdata)
}

func (t *task2) setdata(tasrunktype define.TaskRespType, realid string,
	value interface{}, setdata string) error {
	keyname := fmt.Sprintf("task:%s:%d:%s:%s", t.id, tasrunktype, realid, setdata)
	switch setdata {
	case taskstatus:

		// status, ok := value.(int)
		// if !ok {
		// 	log.Error("value not can change int", zap.Any("data", taskstatus))
		// 	return errors.New("value type error")
		// }
		err := t.redis.Set(keyname, int(value.(define.TaskStatus)), 0).Err()
		if err != nil {
			log.Error("t.redis.Set", zap.Error(err))
			return fmt.Errorf("t.redis.Set failed: %w", err)
		}
	case taskresp:
		content, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("json.Marshal failed: %w", err)
		}
		err = t.redis.Set(keyname, content, 0).Err()
		if err != nil {
			return fmt.Errorf("t.redis.Set failed: %w", err)
		}
	case taskrealtasklog:
		err := t.redis.RPush(keyname, value).Err()
		if err != nil {
			return fmt.Errorf("t.redis.RPush failed: %w", err)
		}

	default:
		log.Error("unknow setdata", zap.String("setdata", setdata))
		return fmt.Errorf("unknow setdata %s", setdata)
	}
	return nil
}

// GetTaskTreeStatatus return task tree status data
func (t *task2) GetTaskTreeStatatus() ([]*define.TaskStatusTree, bool, error) {
	dependtasks, err := t.gettaskinfos()

	if err != nil {
		return nil, false, fmt.Errorf("t.gettaskinfos failed: %w", err)
	}
	log.Debug("alltaskinfos", zap.Strings("tasks", dependtasks))

	retTasksStatus := define.GetTasksTreeStatus()
	var (
		setStatus bool
		// childset  bool
		// task is run finish
		finish = true
	)
	for _, keyname := range dependtasks {
		// keyname
		// task:masterid:taskruntype:realid
		sp := strings.Split(keyname, ":")
		if len(sp) != 4 {
			log.Error("keyname is not 4", zap.String("keuname", keyname))
			continue
		}
		id := sp[3]
		taskruntype, err := strconv.Atoi(sp[2])
		if err != nil {
			log.Error("strconv.Atoi taskruntype column failed", zap.Error(err))
			continue
		}

		statusres, err := t.getdata(define.TaskRespType(taskruntype), id, taskstatus)
		if err != nil {
			log.Error("t.getdata failed", zap.Error(err))
			continue
		}

		// check task is finish
		if statusres == define.TsRun || statusres == define.TsWait {
			finish = false
		}

		task, exist := Cron2.gettask(id)
		if !exist {
			log.Error("get task failed from cacheSchedule",
				zap.String("taskid", id), zap.Error(err))
			continue
		}
		taskTree := define.TaskStatusTree{
			Name:     task.name,
			ID:       id,
			TaskType: define.TaskRespType(taskruntype),
			Status:   statusres.(define.TaskStatus).String(),
		}
		switch define.TaskRespType(taskruntype) {
		case define.ParentTask:
			// 如果有任务是run或者fail或者取消状态就设置任务的状态为这
			// 否则任务就会相同
			if !setStatus {
				if taskTree.Status == define.TsCancel.String() ||
					taskTree.Status == define.TsRun.String() ||
					taskTree.Status == define.TsFail.String() {
					retTasksStatus[0].Status = taskTree.Status
					setStatus = true
				} else {
					retTasksStatus[0].Status = taskTree.Status
				}
			}

			retTasksStatus[0].Children = append(retTasksStatus[0].Children, &taskTree)
		case define.MasterTask:
			retTasksStatus[1].Status = taskTree.Status
			retTasksStatus[1].ID = taskTree.ID
			retTasksStatus[1].Name = taskTree.Name
			setStatus = false
		case define.ChildTask:
			if !setStatus {
				if taskTree.Status == define.TsCancel.String() ||
					taskTree.Status == define.TsRun.String() ||
					taskTree.Status == define.TsFail.String() {
					retTasksStatus[2].Status = taskTree.Status
					setStatus = true
				} else {
					retTasksStatus[2].Status = taskTree.Status
				}
			}
			retTasksStatus[2].Children = append(retTasksStatus[2].Children, &taskTree)
		default:
			log.Error("unsupport task run type", zap.Any("taskruntype", taskruntype))
		}
	}
	return retTasksStatus, finish, nil
}

// GetTaskRealLog return a channel task real log
func (t *task2) GetTaskRealLog(taskruntype define.TaskRespType, realid string, offset int64) ([]byte, error) {
	// 返回一个日志的channel
	// 循环读取记录任务日志的列表然后将日志写到channel中
	// offset 为日志的偏移量每次取日志的offset,offset+1
	// 如果取到了日志就直接返回，如果取出的日志为空并且任务还未运行结束(完成、失败、取消）则返回io.EOF

	keyname := fmt.Sprintf("task:%s:%d:%s:%s", t.id, taskruntype, realid, taskrealtasklog)
	var output []byte
	err := t.redis.LIndex(keyname, offset).Scan(&output)
	if err != nil {
		// 如果不为nil则直接返回错误
		if err != redis.Nil {
			return nil, err
		}
		// 此时未取到新的日志，接下来判断任务的状态
		// 如果任务状态不是运行状态则此次取日志结束，返回io.EOF
		// 获取任务状态
		tsret, tserr := t.getdata(taskruntype, realid, taskstatus)
		if tserr != nil {
			return nil, fmt.Errorf("getdata failed: %w", tserr)
		}

		switch tsret.(define.TaskStatus) {
		case define.TsFinish, define.TsCancel, define.TsFail:
			// 任务已经运行结束，返回结束标志EOF
			return nil, io.EOF
		default:
			return nil, ErrNoGetLog
		}
	}
	return output, nil
}

// cleantaskinfos return task's parent child id
func (t *task2) cleantaskinfos() {
	log.Debug("start clean old key data", zap.String("task", t.name))
	taskinfos := "task:" + t.id
	var res []string
	err := t.redis.LRange(taskinfos, 0, 1).ScanSlice(&res)
	if err != nil {
		log.Error("t.redis.LRange failed:", zap.Error(err))
		return
	}
	for _, key := range res {
		t.redis.Del(key + ":" + taskrealtasklog)
		t.redis.Del(key + ":" + taskresp)
		t.redis.Del(key + ":" + taskstatus)
	}
	t.redis.Del(taskinfos)
	return
}

// gettaskinfos return task's parent child id
func (t *task2) gettaskinfos() ([]string, error) {
	taskinfos := "task:" + t.id
	var res []string
	err := t.redis.LRange(taskinfos, 0, -1).ScanSlice(&res)
	if err != nil {
		return nil, fmt.Errorf("t.redis.LRange failed: %w", err)
	}
	if len(res) == 0 {
		fmt.Println("--------------")
		fmt.Println(t.redis.Keys("task*").Val())
		fmt.Println("--------------")
		return nil, errors.New("get key taskinfos is empty")
	}
	return res, nil
}

func (t *task2) addtaskinfo(taskruntype define.TaskRespType, realid string) error {
	// 初始化任务状态
	// key格式为task:主任务ID:任务的类型:运行任务ID
	// 主任务ID就是触发此次运行任务的ID
	// 任务类型就是这个任务是父任务、子任务还是主任务
	// 运行任务就是实际运行运行的任务
	// task:masterid:taskruntype:realid

	taskinfos := "task:" + t.id
	keyname := fmt.Sprintf("task:%s:%d:%s", t.id, taskruntype, realid)
	t.once.Do(func() {
		// 清除运行的任务
		err := t.redis.Del(taskinfos).Err()
		if err != nil {
			log.Error("once.Do t.redis.Del failed", zap.Error(err))
		}
	})
	err := t.redis.RPush(taskinfos, keyname).Err()
	if err != nil {
		return fmt.Errorf("t.redis.SAdd failed: %w", err)
	}

	// 初始化任务状态
	err = t.setdata(taskruntype, realid, define.TsWait, taskstatus)
	if err != nil {
		return fmt.Errorf("t.setdata failed: %w", err)
	}

	// 清空存储日志list
	err = t.resettasklog(taskruntype, realid)
	if err != nil {
		return fmt.Errorf("t.resettasklog failed: %w", err)
	}
	return err
}

// resettasklog delete log list
func (t *task2) resettasklog(tasrunktype define.TaskRespType, realid string) error {
	keyname := fmt.Sprintf("task:%s:%d:%s:%s", t.id, tasrunktype, realid, taskrealtasklog)
	return t.redis.Del(keyname).Err()
}

// getruntaskdata get runningtask
func (t *task2) getruntaskdata() (*define.RunTask, error) {
	// task:running
	rtasks := "task:running"

	// task:running:id
	rtask := rtasks + ":" + t.id
	res, err := t.redis.Get(rtask).Bytes()
	if err != nil {
		return nil, fmt.Errorf("t.redis.Get failed: %w", err)
	}
	runtask := define.RunTask{}
	err = json.Unmarshal(res, &runtask)
	if err != nil {
		return nil, fmt.Errorf("json.Unmarshal failed: %w", err)
	}
	return &runtask, nil
}

// savetasklog save running task
func (t *task2) savetasklog() error {
	runtask, err := t.getruntaskdata()
	if err != nil {
		log.Error("get task info failed", zap.Error(err))
		return fmt.Errorf("t.gettaskinfo failed: %w", err)
	}

	tasklogres := &define.Log{
		Name:        t.name,
		RunByTaskID: t.id,
		StartTime:   runtask.StartTime,
		EndTime:     time.Now().UnixNano() / 1e6,
		Trigger:     runtask.Trigger,
		Triggerstr:  runtask.Trigger.String(),
		ErrCode:     t.errCode,
		ErrMsg:      t.errMsg,
		ErrTasktype: t.errTasktype,
		ErrTaskID:   t.errTaskID,
		ErrTask:     t.errTask,
		TaskResps:   make([]*define.TaskResp, 0),
	}
	tasklogres.TotalRunTime = int(tasklogres.EndTime - tasklogres.StartTime)

	if t.errTaskID == "" {
		tasklogres.Status = 1
	} else {
		tasklogres.Status = -1
	}

	tasks, err := t.gettaskinfos()
	if err != nil {
		log.Error("t.getttaskinfos failed", zap.Error(err))
		return err
	}
	for _, keyname := range tasks {
		//task:masterid:taskruntype:realid

		// taskresp
		// logdata
		// task status

		sp := strings.Split(keyname, ":")
		if len(sp) != 4 {
			log.Error("keyname parse failed", zap.String("failedkeyname", keyname))
			continue
		}
		i, err := strconv.Atoi(sp[2])
		if err != nil {
			log.Error("get taks run type failed", zap.String("keyname", keyname), zap.Error(err))
			continue
		}

		taskresp, err := t.getdata(define.TaskRespType(i), sp[3], taskresp)
		if err != nil {
			log.Error("t.getdata task resp failed", zap.Error(err))
			continue
		}

		taskstatus, err := t.getdata(define.TaskRespType(i), sp[3], taskstatus)
		if err != nil {
			log.Error("t.getdata task status failed", zap.Error(err))
			continue
		}

		tasklog, err := t.getdata(define.TaskRespType(i), sp[3], taskrealtasklog)
		if err != nil {
			log.Error("t.getdata task log failed", zap.Error(err))
			continue
		}

		tr := taskresp.(define.TaskResp)
		tr.LogData = tasklog.(string)

		if taskstatus.(define.TaskStatus) == define.TsWait {
			tr.Status = define.TsCancel.String()
		} else {
			tr.Status = taskstatus.(define.TaskStatus).String()
		}

		tasklogres.TaskResps = append(tasklogres.TaskResps, &tr)
	}

	t.cleantaskinfos()
	go alarm.JudgeNotify(tasklogres)
	go func() {
		err = model.SaveLog(context.Background(), tasklogres)
		if err != nil {
			log.Error("save task log failed", zap.Error(err))
		}
	}()
	return nil
}

func (t *task2) writelog(tasrunktype define.TaskRespType, realid string, value []byte) {
	err := t.setdata(tasrunktype, realid, value, taskrealtasklog)
	if err != nil {
		log.Error("t.setdata failed", zap.Error(err))
	}
}

// writelogt save log with time
func (t *task2) writelogt(tasrunktype define.TaskRespType, realid, tmpl string, args ...interface{}) {
	value := time.Now().Local().Format("2006-01-02 15:04:05: ") + fmt.Sprintf(tmpl, args...) + "\n"
	err := t.setdata(tasrunktype, realid, value, taskrealtasklog)
	if err != nil {
		log.Error("t.setdata failed", zap.Error(err))
	}
}

// getreturncode get task resp code
func (t *task2) getreturncode(tasrunktype define.TaskRespType, realid string) (int, error) {
	keyname := fmt.Sprintf("task:%s:%d:%s:%s", t.id, tasrunktype, realid, taskrealtasklog)
	// 返回最右的值取后5位，然后放入
	res, err := t.redis.LIndex(keyname, -1).Bytes()
	if err != nil {
		return tasktype.DefaultExitCode, err
	}

	if len(res) >= 5 {
		codebyte := res[len(res)-5:]
		code, err := strconv.Atoi(strings.TrimSpace(string(codebyte)))
		if err != nil {
			// if err != nil ,it is bug
			log.Error("Change str to int failed", zap.Error(err))
			return tasktype.DefaultExitCode, err
		}
		return code, nil
	}
	// if code run there,this is bug
	log.Error("thia is bug,recv buf is nether than 5, get code failed")
	return tasktype.DefaultExitCode, err
}

// getlock get task
func (t *task2) getlock(randstr string) (bool, error) {
	log.Debug("start get lock", zap.String("taskid", t.id))
	lockid := "task:runlock:" + t.id
	set, err := t.redis.SetNX(lockid, randstr, t.cronsub).Result()
	if err != nil {
		log.Error("redis.SetNX failed", zap.Error(err))
		return false, err
	}
	if !set {
		log.Warn("can get run lock", zap.String("taskid", t.id))
		return false, nil
	}
	return true, nil
}

func (t *task2) releaselock(randid string) {
	log.Debug("start release lock", zap.String("taskid", t.id))
	lockid := "task:runlock:" + t.id
	script := redis.NewScript(`
		if redis.call("get",KEYS[1]) == ARGV[1] then
			return redis.call("del",KEYS[1])
		else
			return 0
		end
	`)
	_, err := script.Run(t.redis, []string{lockid}, randid).Result()
	if err != nil {
		log.Error("run delete script failed", zap.Error(err))
	}
}

func (t *task2) islock() (bool, error) {
	lockid := "task:runlock:" + t.id
	// 判断任务是否正在运行，如果正在运行就忽略本次运行
	run, err := t.redis.Exists(lockid).Result()
	if err != nil {
		log.Error("redis.Exists failed", zap.String("key", "running:"+t.id), zap.Error(err))
		return false, err
	}
	if run == 0 {
		return false, nil
	}
	return true, nil
}

// RunTask start run task
func (t *task2) StartRun(trigger define.Trigger) {

	lockid := "task:runlock:" + t.id
	ok, err := t.islock()

	if err != nil {
		log.Error("t.islock failed", zap.Error(err))
		return
	}
	if ok {
		log.Warn("ignore run task,because this task is running", zap.String("taskname", t.name))
		return
	}
	log.Info("start run task", zap.String("taskname", t.name))

	rand.Seed(time.Now().UnixNano())
	randstr := strconv.FormatInt(time.Now().UnixNano()/int64(rand.Int()), 10)

	// 开始抢锁，如果抢到就继续运行任务
	// 为了减少时间差带来获取锁的问题，在获取锁前随机停止0-10毫秒毫秒
	// time.Sleep(time.Microsecond * time.Duration(rand.Int()%100))
	ok, err = t.getlock(randstr)
	if err != nil {
		log.Error("t.getlock failed", zap.Error(err))
		return
	}
	if !ok {
		log.Warn("can not get lock", zap.String("taskname", t.name))
		return
	}

	defer t.releaselock(randstr)

	stopexpire := make(chan struct{})

	// 启动一个协程 定时给锁续期直到删除锁
	go func() {
		ticker := time.NewTicker(t.cronsub * 3 / 4)
		for {
			select {
			case <-stopexpire:
				log.Debug("stop expire lock", zap.String("lockid", lockid))
				ticker.Stop()
				return
			case <-ticker.C:
				if t.cronsub >= time.Second {
					t.redis.Expire(lockid, t.cronsub)
				} else {
					t.redis.PExpire(lockid, t.cronsub)
				}

			}
		}
	}()
	// 退出续约
	defer func() {
		close(stopexpire)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	// save control ctx
	t.ctxcancel = cancel
	defer cancel()
	// 保存运行中的任务
	runningtask := define.RunTask{
		ID:        t.id,
		Name:      t.name,
		Cronexpr:  t.cronexpr,
		StartTime: time.Now().UnixNano() / 1e6,
		Trigger:   trigger,
	}

	Cron2.saverunningtask(&runningtask)
	defer func() {
		Cron2.removerunningtask(&runningtask)
	}()

	task, err := model.GetTaskByID(context.Background(), t.id)
	switch err.(type) {
	case nil:
		goto Next
	case define.ErrNotExist:
		log.Error("task is not exist", zap.String("taskid", t.id))
		return
	default:
		log.Error("model.GetTaskByID failed", zap.String("taskid", t.id), zap.Error(err))
		return
	}
Next:

	// 保存一个任务的父子任务的信息
	// 实时日志 :reallog list
	// 状态 :status set
	// 任务返回数据 :taskresp set
	t.once = sync.Once{}

	// 初始化所有的任务
	pos := 1
	for _, parenttaskid := range task.ParentTaskIds {
		err = t.addtaskinfo(define.ParentTask, parenttaskid)
		if err != nil {
			log.Error("t.addtaskinfo failed", zap.Error(err))
			return
		}
		pos++
	}
	err = t.addtaskinfo(define.MasterTask, t.id)
	if err != nil {
		log.Error("t.addtaskinfo failed", zap.Error(err))
		return
	}
	pos++
	for _, childtaskid := range task.ChildTaskIds {
		err = t.addtaskinfo(define.ChildTask, childtaskid)
		if err != nil {
			log.Error("t.addtaskinfo failed", zap.Error(err))
			return
		}
		pos++
	}

	t.errTaskID = ""
	t.errTask = ""
	t.errCode = 0
	t.errMsg = ""
	t.errTasktype = 0

	// if exist a err task,will stop all task
	g := errgroup.WithCancel(ctx)
	g.GOMAXPROCS(1)
	// parent tasks
	g.Go(func(ctx context.Context) error {
		return t.runMultiTasks(ctx, task.ParentRunParallel, define.ParentTask, task.ID, task.ParentTaskIds...)
	})
	// master task
	g.Go(func(ctx context.Context) error {
		return t.runTask(ctx, task.ID, define.MasterTask)
	})
	// childs task
	g.Go(func(ctx context.Context) error {
		return t.runMultiTasks(ctx, task.ChildRunParallel, define.ChildTask, task.ID, task.ChildTaskIds...)
	})
	err = g.Wait()
	if err != nil {
		log.Error("task run failed", zap.String("taskid", t.id), zap.Error(err))
	}

	err = t.savetasklog()
	if err != nil {
		log.Error("t.savetasklog failed", zap.Error(err))
	}
}

// run multi tasks
// if hash one task err, will exit all task
// TODO: task run err whether influence  other task
func (t *task2) runMultiTasks(ctx context.Context, RunParallel bool,
	tasktype define.TaskRespType, runbyid string, taskids ...string) error {
	if len(taskids) == 0 {
		return nil
	}
	var maxproc int
	if RunParallel {
		maxproc = len(taskids)
	} else {
		maxproc = 1
	}
	g := errgroup.WithCancel(ctx)
	g.GOMAXPROCS(maxproc)
	for _, id := range taskids {
		taskid := id
		g.Go(func(ctx context.Context) error {
			return t.runTask(ctx, taskid, tasktype)
		})
	}
	return g.Wait()

}

// runTask start run task,log will store
func (t *task2) runTask(ctx context.Context, /*real run task id*/
	id string, taskruntype define.TaskRespType) error {
	var (
		// error
		err error
		// task data
		taskdata *define.GetTask
		realtask *task2
		ok       bool

		tdata []byte
		conn  *grpc.ClientConn
		// recv grpc stream
		taskrespstream pb.Task_RunTaskClient
		// grpc client
		taskclient pb.TaskClient
		taskreq    *pb.TaskReq
		// recv grpc stream
		pbtaskresp *pb.TaskResp

		ctxcancel context.CancelFunc
		taskctx   context.Context
		output    []byte

		taskrespcode = tasktype.DefaultExitCode
	)
	// TODO 故障转移

	// set task is running
	t.setdata(taskruntype, id, define.TsRun, taskstatus)

	queryctx, querycancel := context.WithTimeout(ctx,
		config.CoreConf.Server.DB.MaxQueryTime.Duration)
	defer querycancel()

	taskdata, err = model.GetTaskByID(queryctx, id)
	if err != nil {
		log.Error("model.GetTaskByID failed", zap.String("taskid", id),
			zap.Error(err))
		t.writelogt(taskruntype, id, "Get %s Task id %s from db failed: %v",
			taskruntype.String(), id, err)
		goto Check
	}
	// 如果异步执行那么任务的状态，控制并发等问题就需要重新设计
	// 双向故障转移，如果Worker节点挂掉，则重新
	realtask, ok = Cron2.gettask(id)
	if !ok {
		log.Error("can not get task", zap.String("taskid", id))
		t.writelogt(taskruntype, id, "Get %s Task id %s from cacheSchedule failed: %v",
			taskruntype.String(), id, err)
		goto Check
	}

	conn, err = tryGetRCCConn(ctx, realtask.next)
	if err != nil {
		log.Error("tryGetRpcConn failed", zap.String("hostgroup", taskdata.HostGroup), zap.Error(err))
		t.writelogt(taskruntype, id, "Get Rpc Conn Failed From Hostgroup %s[%s] Err: %v",
			taskdata.HostGroup, taskdata.HostGroupID, err)
		goto Check
	}
	// defer conn.Close()

	t.writelogt(taskruntype, id, "start run task %s[%s] on host %s", taskdata.Name, taskdata.ID, conn.Target())
	tdata, err = json.Marshal(taskdata.TaskData)
	if err != nil {
		log.Error("json.Marshal", zap.Error(err))
		t.writelogt(taskruntype, id, "task %s json.Marshal value:%+v failed :%+v", taskdata.Name, taskdata.TaskData, err)
		goto Check
	}

	// task run data
	taskreq = &pb.TaskReq{
		TaskId:   id,
		TaskType: int32(taskdata.TaskType),
		TaskData: tdata,
	}

	// taskctx only use RunTask
	if taskdata.Timeout > 0 {
		taskctx, ctxcancel = context.WithTimeout(ctx, time.Second*time.Duration(taskdata.Timeout))
	} else {
		taskctx, ctxcancel = context.WithCancel(ctx)
	}
	defer ctxcancel()

	// defer ctxcancel()
	taskclient = pb.NewTaskClient(conn)

	taskrespstream, err = taskclient.RunTask(taskctx, taskreq)
	if err != nil {
		log.Error("Run task failed", zap.Error(err))
		t.writelogt(taskruntype, id, "Run Task %s[%s] TaskData [%v] failed:%v", taskdata.Name, id, taskreq, err)
		goto Check
	}

	t.writelogt(taskruntype, id, "task %s[%s]  output----------------", taskdata.Name, id)
	for {
		// Recv return err is nil or io.EOF
		// the last lastrecv must be return code 3 byte
		pbtaskresp, err = taskrespstream.Recv()
		if err != nil {
			if err == io.EOF {
				err = nil
				// 获取返回码
				taskrespcode, err = t.getreturncode(taskruntype, id)
				goto Check
			}
			log.Error("recv task stream failed", zap.Error(err))
			err = DealRPCErr(err)
			if err.Error() == resp.GetMsgErr(resp.ErrRPCUnavailable).Error() {
				// worker host is down,so we need run this fail task again
				log.Error("worker host is down, run task again", zap.String("taskid", id))
				t.writelogt(taskruntype, id, "worker host %s is down,so run task %s again", conn.Target(), taskdata.Name)
				return t.runTask(ctx, id, taskruntype)
			}
			t.writelogt(taskruntype, id, "Task %s[%s] Run Fail: %v", taskdata.Name, id, err.Error())
			// Alarm
			log.Error("recv failed", zap.Error(err))
			// err = resp.GetMsgErr(taskrespcode)
			goto Check
		}
		t.writelog(taskruntype, id, pbtaskresp.GetResp())
		output = append(output, pbtaskresp.GetResp()...)
	}
Check:

	// 存储任务结果
	tmptaskresp := define.TaskResp{
		TaskID:   id,
		Task:     realtask.name,
		Code:     taskrespcode,
		TaskType: taskruntype,
	}
	if conn != nil {
		// if conn worker failed,can not get worker host
		tmptaskresp.RunHost = conn.Target()
	}
	t.setdata(taskruntype, id, tmptaskresp, taskresp)
	// 处理错误需要加锁
	// 如果有一个任务失败就取消其他的任务

	t.Lock()
	defer t.Unlock()
	if err != nil && t.errTaskID != "" {
		select {
		case <-ctx.Done():
			log.Error("task is cancel", zap.String("task", realtask.name))
			t.writelogt(taskruntype, id, "task %s[%s] is canceled", realtask.name, id)
			t.setdata(taskruntype, id, define.TsCancel, taskstatus)
			return nil
		default:
		}
	}
	var alarmerr error
	// if a task fail other task will return context.Canceled,but it can not alarm
	// because the first err task always alarm,so other task do not alarm
	// and the first err task's errmsg will save tasking

	// check task resp code and resp content
	judgeres := func() error {
		if err != nil {
			return err
		}
		if taskdata.ExpectCode != taskrespcode {
			return fmt.Errorf("%s task %s[%s] resp code is %d,want resp code %d", taskruntype.String(), id, taskdata.Name, taskrespcode, taskdata.ExpectCode)
		}
		if taskdata.ExpectContent != "" {
			if !strings.Contains(string(output), taskdata.ExpectContent) {
				return fmt.Errorf("%s task %s[%s] resp context not contains expect content: %s", taskruntype.String(), id, taskdata.Name, taskdata.ExpectContent)
			}
		}
		return nil
	}
	alarmerr = judgeres()

	if alarmerr != nil {
		// 第一个失败的任务会运行到此处
		log.Error("task run fail", zap.String("task", realtask.name), zap.Error(err))
		if t.errTaskID == "" {
			// runbytask.status = -1
			t.errTaskID = id
			t.errTask = realtask.name
			t.errCode = taskrespcode
			t.errMsg = alarmerr.Error()
			t.errTasktype = taskruntype
			t.setdata(taskruntype, id, define.TsFail, taskstatus)
		}
	} else {
		log.Debug("task run success", zap.String("task", realtask.name))
		t.setdata(taskruntype, id, define.TsFinish, taskstatus)
		// 如有任务失败，那么还未运行的任务可以标记为取消
	}
	return alarmerr
}

// cacheSchedule2 save task status
type cacheSchedule2 struct {
	sync.RWMutex
	redis *redis.Client
	ts    map[string]*task2
}

// Init2 start run already exists task from db
func Init2() error {
	client := redis.NewClient(&redis.Options{
		Addr:     config.CoreConf.Server.Redis.Addr,
		Password: config.CoreConf.Server.Redis.PassWord,
	})

	err := client.Ping().Err()
	if err != nil {
		log.Error("connect redis failed", zap.String("addr", config.CoreConf.Server.Redis.Addr))
		return err
	}

	Cron2 = &cacheSchedule2{
		ts:    make(map[string]*task2),
		redis: client,
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		config.CoreConf.Server.DB.MaxQueryTime.Duration)
	defer cancel()
	isinstalll, err := model.QueryIsInstall(ctx)
	if err != nil {
		log.Error("model.QueryIsInstall failed", zap.Error(err))
		return fmt.Errorf("model.QueryIsInstall failed: %w", err)
	}
	if !isinstalll {
		log.Debug("Crocodile is Not Install")
		return nil
	}
	eps, _, err := model.GetTasks(ctx, 0, 0, "", "", "")
	if err != nil {
		log.Error("GetTasks failed", zap.Error(err))
		return err
	}
	log.Debug("start init task", zap.Int("task", len(eps)))
	for _, t := range eps {
		Cron2.addtask(t.ID, t.Name, t.Cronexpr, GetRoutePolicy(t.HostGroupID, t.RoutePolicy), t.Run)
	}

	go RecvEvent()
	log.Info("init task success", zap.Int("Total", len(eps)))
	return nil
}

// Add task to schedule
func (s *cacheSchedule2) addtask(taskid, taskname string, cronExpr string, next Next, canrun bool) {
	log.Debug("start add task", zap.String("taskid", taskid), zap.String("taskname", taskname))

	oldtask, exist := s.gettask(taskid)
	if exist {
		close(oldtask.close)
		if oldtask.ctxcancel != nil {
			oldtask.ctxcancel()
		}
		delete(s.ts, taskname)
	}
	t := task2{
		id:       taskid,
		name:     taskname,
		cronexpr: cronExpr,
		close:    make(chan struct{}),
		next:     next,
		canrun:   canrun,
		redis:    s.redis,
	}
	s.Lock()
	s.ts[taskid] = &t
	s.Unlock()
	go s.runSchedule(taskid)
}

// Del schedule task
// if delete taskid,this taskid must be remove from other task's child or parent
func (s *cacheSchedule2) deletetask(taskid string) {
	log.Info("start delete task", zap.String("taskid", taskid))

	task, exist := s.gettask(taskid)

	if exist {
		log.Debug("start clean ", zap.String("id", taskid))
		s.Lock()
		delete(s.ts, taskid)
		s.Unlock()
		if task.ctxcancel != nil {
			task.ctxcancel()
		}
		defer func() {
			recover()
		}()
		close(task.close)
	}
}

// killTask will stop running task
func (s *cacheSchedule2) killtask(taskid string) {
	task, exist := s.gettask(taskid)
	if !exist {
		log.Warn("stoptask failed,task is not exist", zap.String("taskid", taskid))
		return
	}
	if task.ctxcancel != nil {
		task.ctxcancel()
	}
}

func (s *cacheSchedule2) runSchedule(taskid string) {
	task, exist := s.gettask(taskid)
	if !exist {
		log.Error("task is not exist in ts", zap.String("taskid", taskid))
		return
	}
	log.Info("start run cronexpr", zap.Any("task", task.name), zap.String("id", taskid))

	expr, err := cronexpr.Parse(task.cronexpr)
	if err != nil {
		log.Error("cronexpr parse failed", zap.Error(err))
		return
	}

	var (
		last time.Time
		next time.Time
	)

	// 计算出锁的续约时间
	task.cronsub = expr.Next(last).Sub(last) / 4
	if task.cronsub > time.Second*30 {
		task.cronsub = time.Second * 30
	}

	for {
		last = time.Now()
		next = expr.Next(last)
		select {
		case <-task.close:
			log.Info("close task Schedule", zap.String("taskid", taskid), zap.Any("name", task.name))
			return
		case <-time.After(next.Sub(last)):
			if !task.canrun {
				log.Warn("task is stop run by auto schedule", zap.String("taskname", task.name), zap.String("taskid", task.id))
				continue
			}
			go task.StartRun(define.Auto)
		}
	}
}

// GetRunningTask return running task
func (s *cacheSchedule2) GetRunningTask() ([]*define.RunTask, error) {
	// task:running
	rtasks := "task:running"
	var rtkeys []string
	err := s.redis.SMembers(rtasks).ScanSlice(&rtkeys)
	if err != nil {
		return nil, err
	}
	var runtasks = runningTask{}
	for _, runningtaskkey := range rtkeys {
		var runtask define.RunTask
		var res []byte
		err = s.redis.Get(runningtaskkey).Scan(&res)
		if err != nil {
			log.Error("Scan runtask failed", zap.Error(err))
			continue
		}
		err = json.Unmarshal(res, &runtask)
		if err != nil {
			log.Error("json.Unmarshal runtask failed", zap.Error(err))
			continue
		}

		runtask.StartTimeStr = utils.UnixToStr(runtask.StartTime / 1e3)
		runtask.RunTime = int(time.Now().UnixNano()/1e6 - runtask.StartTime)
		runtask.TriggerStr = runtask.Trigger.String()

		ok, err := s.IsRunning(runtask.ID)
		if err != nil {
			log.Error("s.IsRunning failed", zap.Error(err))
			if strings.HasPrefix(err.Error(), "can not get taskid") {
				// removerunningtask未执行，调度节点挂掉，所以就一直保留
				// 如果到这里就直接删掉
				Cron2.removerunningtask(&define.RunTask{ID: runtask.ID})

			}
			continue
		}
		if !ok {
			// log.Warn("task lock is not exists", zap.String("taskname", runtask.Name))
			continue
		}
		runtasks = append(runtasks, &runtask)
	}
	sort.Sort(runtasks)
	return runtasks, nil
}

// IsRunning check task lock
func (s *cacheSchedule2) IsRunning(taskid string) (bool, error) {
	t, ok := s.gettask(taskid)
	if !ok {
		return false, fmt.Errorf("can not get taskid %s", taskid)
	}
	return t.islock()
}

// saverunningtask save running task
func (s *cacheSchedule2) saverunningtask(runningtask *define.RunTask) error {
	// 首先存储到运行中任务集合，然后再保存运行的数据

	// task:running
	rtasks := "task:running"

	// task:running:id
	rtask := rtasks + ":" + runningtask.ID

	res, err := json.Marshal(runningtask)
	if err != nil {
		return fmt.Errorf("json.Marshal failed: %w", err)
	}

	pipeline := s.redis.Pipeline()
	err = pipeline.SAdd(rtasks, rtask).Err()
	if err != nil {
		return fmt.Errorf("pipeline.SAdd failed: %w", err)
	}
	err = pipeline.Set(rtask, res, 0).Err()
	if err != nil {
		return fmt.Errorf("pipeline.Set failed: %w", err)
	}
	_, err = pipeline.Exec()
	if err != nil {
		return fmt.Errorf("pipeline.Exec failed: %w", err)
	}
	return nil
}

// removerunningtask remove running task
func (s *cacheSchedule2) removerunningtask(runningtask *define.RunTask) error {
	// task:running
	rtasks := "task:running"

	// task:running:id
	rtask := rtasks + ":" + runningtask.ID

	pipeline := s.redis.Pipeline()
	err := pipeline.SRem(rtasks, rtask).Err()
	if err != nil {
		return fmt.Errorf("pipeline.SRem failed: %w", err)
	}
	err = pipeline.Del(rtask).Err()
	if err != nil {
		return fmt.Errorf("pipeline.Del failed: %w", err)
	}
	_, err = pipeline.Exec()
	if err != nil {
		return fmt.Errorf("pipeline.SAdd failed: %w", err)
	}
	return nil
}

// GetTask return task2
func (s *cacheSchedule2) GetTask(taskid string) (*task2, bool) {
	return s.gettask(taskid)
}

func (s *cacheSchedule2) gettask(taskid string) (*task2, bool) {
	s.RLock()
	t, ok := s.ts[taskid]
	s.RUnlock()
	return t, ok
}

func (s *cacheSchedule2) PubTaskEvent(eventdata []byte) {
	s.redis.Publish(pubsubChannel, eventdata)
}
