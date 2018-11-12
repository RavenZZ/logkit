package tailx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/json-iterator/go"

	"github.com/qiniu/log"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/reader"
	. "github.com/qiniu/logkit/reader/config"
	. "github.com/qiniu/logkit/utils/models"
)

var (
	_ reader.DaemonReader = &Reader{}
	_ reader.StatsReader  = &Reader{}
	_ reader.LagReader    = &Reader{}
	_ reader.Reader       = &Reader{}
	_ Resetable           = &Reader{}
)

func init() {
	reader.RegisterConstructor(ModeTailx, NewReader)
}

type Reader struct {
	meta *reader.Meta
	// Note: 原子操作，用于表示 reader 整体的运行状态
	status int32

	stopChan chan struct{}
	msgChan  chan Result
	errChan  chan error

	stats     StatsInfo
	statsLock sync.RWMutex

	fileReaders map[string]*ActiveReader
	armapmux    sync.Mutex
	currentFile string
	headRegexp  *regexp.Regexp
	cacheMap    map[string]string

	//以下为传入参数
	logPathPattern       string
	ignoreLogPathPattern string
	expire               time.Duration
	submetaExpire        time.Duration
	statInterval         time.Duration
	maxOpenFiles         int
	whence               string

	notFirstTime bool
}

type ActiveReader struct {
	cacheLineMux sync.RWMutex
	br           *reader.BufReader
	realpath     string
	originpath   string
	readcache    string
	msgchan      chan<- Result
	errChan      chan<- error
	status       int32
	inactive     int32 //当inactive>0 时才会被expire回收
	runnerName   string

	emptyLineCnt int

	stats     StatsInfo
	statsLock sync.RWMutex
}

type Result struct {
	result  string
	logpath string
}

func NewActiveReader(originPath, realPath, whence string, notFirstTime bool, meta *reader.Meta, msgChan chan<- Result, errChan chan<- error) (ar *ActiveReader, err error) {
	rpath := strings.Replace(realPath, string(os.PathSeparator), "_", -1)
	if runtime.GOOS == "windows" {
		rpath = strings.Replace(rpath, ":", "_", -1)
	}
	subMetaPath := filepath.Join(meta.Dir, rpath)
	subMeta, err := reader.NewMetaWithRunnerName(meta.RunnerName, subMetaPath, subMetaPath, realPath, ModeFile, meta.TagFile, reader.DefautFileRetention)
	if err != nil {
		return nil, err
	}
	subMeta.SetEncodingWay(meta.GetEncodingWay())
	subMeta.Readlimit = meta.Readlimit
	isNewFile := meta.IsStatisticFileExist() || notFirstTime //是否为存量文件
	if isNewFile && subMeta.IsNotExist() {
		whence = WhenceOldest // 非存量文件第一次读取时从头开始读
	}

	//tailx模式下新增runner是因为文件已经感知到了，所以不可能文件不存在，那么如果读取还遇到错误，应该马上返回，所以errDirectReturn=true
	fr, err := reader.NewSingleFile(subMeta, realPath, whence, true)
	if err != nil {
		return
	}
	bf, err := reader.NewReaderSize(fr, subMeta, reader.DefaultBufSize)
	if err != nil {
		//如果没有创建成功，要把reader close掉，否则会因为ratelimit导致线程泄露
		fr.Close()
		return
	}
	return &ActiveReader{
		cacheLineMux: sync.RWMutex{},
		br:           bf,
		realpath:     realPath,
		originpath:   originPath,
		msgchan:      msgChan,
		errChan:      errChan,
		inactive:     1,
		emptyLineCnt: 0,
		runnerName:   meta.RunnerName,
		status:       StatusInit,
		statsLock:    sync.RWMutex{},
	}, nil

}

func (ar *ActiveReader) Start() {
	if atomic.LoadInt32(&ar.status) == StatusRunning {
		log.Warnf("Runner[%v] ActiveReader %s was already running", ar.runnerName, ar.originpath)
		return
	}

	if atomic.LoadInt32(&ar.status) == StatusStopping {
		cnt := 0
		// 等待结束
		for atomic.LoadInt32(&ar.status) != StatusStopped {
			cnt++
			//超过300个10ms，即3s，就强行退出
			if cnt > 300 {
				log.Errorf("Runner[%v] ActiveReader %s was not closed after 3s, force closing it", ar.runnerName, ar.originpath)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		atomic.CompareAndSwapInt32(&ar.status, StatusStopping, StatusStopped)
		log.Warnf("Runner[%v] ActiveReader %s was stopped", ar.runnerName, ar.originpath)
	}

	atomic.StoreInt32(&ar.status, StatusInit)

	go ar.Run()
}

func (ar *ActiveReader) Stop() error {
	if atomic.LoadInt32(&ar.status) == StatusStopped {
		return nil
	}

	if !atomic.CompareAndSwapInt32(&ar.status, StatusRunning, StatusStopping) &&
		atomic.LoadInt32(&ar.status) != StatusStopping {
		err := fmt.Errorf("Runner[%v] ActiveReader %s was not in StatusRunning or StatusStopping status, exit it... ", ar.runnerName, ar.originpath)
		log.Debug(err)
		return err
	} else {
		if !IsSelfRunner(ar.runnerName) {
			log.Warnf("Runner[%v] ActiveReader %s was closing", ar.runnerName, ar.originpath)
		} else {
			log.Debugf("Runner[%v] ActiveReader %s was closing", ar.runnerName, ar.originpath)
		}
	}

	cnt := 0
	// 等待结束
	for atomic.LoadInt32(&ar.status) != StatusStopped {
		cnt++
		//超过3个1s，即3s，就强行退出
		if cnt > 3 {
			log.Errorf("Runner[%v] ActiveReader %s was not closed after 3s, force closing it", ar.runnerName, ar.originpath)
			break
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

func (ar *ActiveReader) Run() {
	if !atomic.CompareAndSwapInt32(&ar.status, StatusInit, StatusRunning) {
		if !IsSelfRunner(ar.runnerName) {
			log.Errorf("Runner[%v] ActiveReader %s was not in StatusInit before Running,exit it...", ar.runnerName, ar.originpath)
		} else {
			log.Debugf("Runner[%v] ActiveReader %s was not in StatusInit before Running,exit it...", ar.runnerName, ar.originpath)
		}
		return
	}
	var err error
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if atomic.LoadInt32(&ar.status) == StatusStopped || atomic.LoadInt32(&ar.status) == StatusStopping {
			atomic.CompareAndSwapInt32(&ar.status, StatusStopping, StatusStopped)
			if !IsSelfRunner(ar.runnerName) {
				log.Warnf("Runner[%v] ActiveReader %s was stopped", ar.runnerName, ar.originpath)
			} else {
				log.Debugf("Runner[%v] ActiveReader %s was stopped", ar.runnerName, ar.originpath)
			}
			return
		}
		if ar.readcache == "" {
			ar.cacheLineMux.Lock()
			ar.readcache, err = ar.br.ReadLine()
			ar.cacheLineMux.Unlock()
			if err != nil && err != io.EOF && err != os.ErrClosed {
				if !IsSelfRunner(ar.runnerName) {
					log.Warnf("Runner[%v] ActiveReader %s read error: %v, stop it", ar.runnerName, ar.originpath, err)
				} else {
					log.Debugf("Runner[%v] ActiveReader %s read error: %v, stop it", ar.runnerName, ar.originpath, err)
				}
				ar.setStatsError(err.Error())
				ar.sendError(err)
				ar.Stop()
				return
			}
			if ar.readcache == "" {
				ar.emptyLineCnt++
				//文件EOF，同时没有任何内容，代表不是第一次EOF，休息时间设置长一些
				if err == io.EOF {
					atomic.StoreInt32(&ar.inactive, 1)
					log.Debugf("Runner[%v] %v meet EOF, ActiveReader was inactive now, stop it", ar.runnerName, ar.originpath)
					ar.Stop()
					return
				}
				// 3s 没读到内容，设置为inactive
				if ar.emptyLineCnt > 3 {
					atomic.StoreInt32(&ar.inactive, 1)
					log.Debugf("Runner[%v] %v meet EOF, ActiveReader was inactive now, stop it", ar.runnerName, ar.originpath)
					ar.Stop()
					return
				}
				//读取的结果为空，无论如何都sleep 1s
				time.Sleep(time.Second)
				continue
			}
		}
		log.Debugf("Runner[%v] %v >>>>>>readcache <%v> linecache <%v>", ar.runnerName, ar.originpath, ar.readcache, string(ar.br.FormMutiLine()))
		repeat := 0
		for {
			if ar.readcache == "" {
				break
			}
			repeat++
			if repeat%3000 == 0 {
				if !IsSelfRunner(ar.runnerName) {
					log.Errorf("Runner[%v] %v ActiveReader has timeout 3000 times with readcache %v", ar.runnerName, ar.originpath, ar.readcache)
				} else {
					log.Debugf("Runner[%v] %v ActiveReader has timeout 3000 times with readcache %v", ar.runnerName, ar.originpath, ar.readcache)
				}
			}

			atomic.StoreInt32(&ar.inactive, 0)
			ar.emptyLineCnt = 0
			//做这一层结构为了快速结束
			if atomic.LoadInt32(&ar.status) == StatusStopped || atomic.LoadInt32(&ar.status) == StatusStopping {
				log.Debugf("Runner[%v] %v ActiveReader was stopped when waiting to send data", ar.runnerName, ar.originpath)
				atomic.CompareAndSwapInt32(&ar.status, StatusStopping, StatusStopped)
				return
			}
			select {
			case ar.msgchan <- Result{result: ar.readcache, logpath: ar.originpath}:
				ar.cacheLineMux.Lock()
				ar.readcache = ""
				ar.cacheLineMux.Unlock()
			case <-timer.C:
			}
		}
	}
}

func (ar *ActiveReader) isStopping() bool {
	return atomic.LoadInt32(&ar.status) == StatusStopping
}

func (ar *ActiveReader) hasStopped() bool {
	return atomic.LoadInt32(&ar.status) == StatusStopped
}

func (ar *ActiveReader) Close() error {
	defer func() {
		if !IsSelfRunner(ar.runnerName) {
			log.Warnf("Runner[%v] ActiveReader %s was closed", ar.runnerName, ar.originpath)
		} else {
			log.Debugf("Runner[%v] ActiveReader %s was closed", ar.runnerName, ar.originpath)
		}
	}()
	brCloseErr := ar.br.Close()
	if err := ar.Stop(); err != nil {
		return brCloseErr
	}
	return nil
}

func (ar *ActiveReader) setStatsError(err string) {
	ar.statsLock.Lock()
	defer ar.statsLock.Unlock()
	ar.stats.LastError = err
}

func (ar *ActiveReader) sendError(err error) {
	if err == nil {
		return
	}
	if ar.isStopping() || ar.hasStopped() {
		err = fmt.Errorf("sendError %v failed as is closed", err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Runner[%v] ActiveReader %s Recovered from %v", ar.runnerName, ar.originpath, r)
		}
	}()
	ar.errChan <- err
}

func (ar *ActiveReader) Status() StatsInfo {
	ar.statsLock.RLock()
	defer ar.statsLock.RUnlock()
	return ar.stats
}

func (ar *ActiveReader) Lag() (rl *LagInfo, err error) {
	return ar.br.Lag()
}

//除了sync自己的bufreader，还要sync一行linecache
func (ar *ActiveReader) SyncMeta() string {
	ar.cacheLineMux.Lock()
	defer ar.cacheLineMux.Unlock()
	ar.br.SyncMeta()
	return ar.readcache
}

func (ar *ActiveReader) expired(expire time.Duration) bool {
	// 如果过期时间为 0，则永不过期
	if expire.Nanoseconds() == 0 {
		return false
	}

	fi, err := os.Stat(ar.realpath)
	if err != nil {
		if os.IsNotExist(err) {
			return true
		}
		if !IsSelfRunner(ar.runnerName) {
			log.Errorf("Runner[%v] stat log %v error %v, will not expire it...", ar.runnerName, ar.originpath, err)
		} else {
			log.Debugf("Runner[%v] stat log %v error %v, will not expire it...", ar.runnerName, ar.originpath, err)
		}
		return false
	}
	if fi.ModTime().Add(expire).Before(time.Now()) && atomic.LoadInt32(&ar.inactive) > 0 {
		return true
	}
	return false
}

func NewReader(meta *reader.Meta, conf conf.MapConf) (reader.Reader, error) {
	logPathPattern, err := conf.GetString(KeyLogPath)
	if err != nil {
		return nil, err
	}
	whence, _ := conf.GetStringOr(KeyWhence, WhenceOldest)

	statIntervalDur, _ := conf.GetStringOr(KeyStatInterval, "3m")
	maxOpenFiles, _ := conf.GetIntOr(KeyMaxOpenFiles, 256)

	expireDur, _ := conf.GetStringOr(KeyExpire, "24h")
	expire, err := time.ParseDuration(expireDur)
	if err != nil {
		return nil, err
	}

	ignoreLogPathPattern, _ := conf.GetStringOr(KeyIgnoreLogPath, "")

	submetaExpireDur, _ := conf.GetStringOr(KeySubmetaExpire, "720h")
	submetaExpire, err := time.ParseDuration(submetaExpireDur)
	if err != nil {
		return nil, err
	}
	// submetaExpire 为 0 时，不清理元数据
	if IsSubmetaExpireValid(submetaExpire, expire) {
		return nil, fmt.Errorf("%q valus is less than %q", KeySubmetaExpire, KeyExpire)
	}

	statInterval, err := time.ParseDuration(statIntervalDur)
	if err != nil {
		return nil, err
	}
	_, _, bufsize, err := meta.ReadBufMeta()
	if err != nil {
		if os.IsNotExist(err) {
			log.Debugf("Runner[%v] %v recover from meta error %v, ignore...", meta.RunnerName, logPathPattern, err)
		} else {
			if !IsSelfRunner(meta.RunnerName) {
				log.Warnf("Runner[%v] %v recover from meta error %v, ignore...", meta.RunnerName, logPathPattern, err)
			} else {
				log.Debugf("Runner[%v] %v recover from meta error %v, ignore...", meta.RunnerName, logPathPattern, err)
			}
		}
		bufsize = 0
		err = nil
	}

	cacheMap := make(map[string]string)
	buf := make([]byte, bufsize)
	if bufsize > 0 {
		if _, err = meta.ReadBuf(buf); err != nil {
			if os.IsNotExist(err) {
				log.Debugf("Runner[%v] read buf file %v error %v, ignore...", meta.RunnerName, meta.BufFile(), err)
			} else {
				if !IsSelfRunner(meta.RunnerName) {
					log.Warnf("Runner[%v] read buf file %v error %v, ignore...", meta.RunnerName, meta.BufFile(), err)
				} else {
					log.Debugf("Runner[%v] read buf file %v error %v, ignore...", meta.RunnerName, meta.BufFile(), err)
				}
			}
		} else {
			err = jsoniter.Unmarshal(buf, &cacheMap)
			if err != nil {
				if !IsSelfRunner(meta.RunnerName) {
					log.Warnf("Runner[%v] Unmarshal read buf cache error %v, ignore...", meta.RunnerName, err)
				} else {
					log.Debugf("Runner[%v] Unmarshal read buf cache error %v, ignore...", meta.RunnerName, err)
				}
			}
		}
		err = nil
	}

	return &Reader{
		meta:                 meta,
		status:               StatusInit,
		stopChan:             make(chan struct{}),
		msgChan:              make(chan Result),
		errChan:              make(chan error),
		logPathPattern:       logPathPattern,
		ignoreLogPathPattern: strings.TrimSpace(ignoreLogPathPattern),
		whence:               whence,
		expire:               expire,
		submetaExpire:        submetaExpire,
		statInterval:         statInterval,
		maxOpenFiles:         maxOpenFiles,
		fileReaders:          make(map[string]*ActiveReader), //armapmux
		cacheMap:             cacheMap,                       //armapmux
	}, nil
}

func (r *Reader) isStopping() bool {
	return atomic.LoadInt32(&r.status) == StatusStopping
}

func (r *Reader) hasStopped() bool {
	return atomic.LoadInt32(&r.status) == StatusStopped
}

func (r *Reader) Name() string {
	return "TailxReader: " + r.logPathPattern
}

func (r *Reader) SetMode(mode string, value interface{}) error {
	reg, err := reader.HeadPatternMode(mode, value)
	if err != nil {
		return fmt.Errorf("get head pattern mode: %v", err)
	}
	if reg != nil {
		r.headRegexp = reg
	}
	return nil
}

func (r *Reader) setStatsError(err string) {
	r.statsLock.Lock()
	defer r.statsLock.Unlock()
	r.stats.LastError = err
}

func (r *Reader) sendError(err error) {
	if err == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf("Reader %q was panicked and recovered from %v", r.Name(), rec)
		}
	}()
	r.errChan <- err
}

// checkExpiredFiles 函数关闭过期的文件，再更新
func (r *Reader) checkExpiredFiles() {
	r.armapmux.Lock()
	defer r.armapmux.Unlock()

	var paths []string
	for path, ar := range r.fileReaders {
		if ar.expired(r.expire) {
			ar.Close()
			delete(r.fileReaders, path)
			delete(r.cacheMap, path)
			r.meta.RemoveSubMeta(path)
			paths = append(paths, path)
		}
	}
	if len(paths) > 0 {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Infof("Runner[%v] expired logpath: %v", r.meta.RunnerName, strings.Join(paths, ", "))
		} else {
			log.Debugf("Runner[%v] expired logpath: %v", r.meta.RunnerName, strings.Join(paths, ", "))
		}
	}
}

func (r *Reader) statLogPath() {
	//达到最大打开文件数，不再追踪
	if len(r.fileReaders) >= r.maxOpenFiles {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Warnf("Runner[%v] %v meet maxOpenFiles limit %v, ignore Stat new log...", r.meta.RunnerName, r.Name(), r.maxOpenFiles)
		} else {
			log.Debugf("Runner[%v] %v meet maxOpenFiles limit %v, ignore Stat new log...", r.meta.RunnerName, r.Name(), r.maxOpenFiles)
		}
		return
	}
	matches, err := filepath.Glob(r.logPathPattern)
	if err != nil {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Errorf("Runner[%v] stat logPathPattern error %v", r.meta.RunnerName, err)
		} else {
			log.Debugf("Runner[%v] stat logPathPattern error %v", r.meta.RunnerName, err)
		}
		r.setStatsError("Runner[" + r.meta.RunnerName + "] stat logPathPattern error " + err.Error())
		return
	}
	if len(matches) > 0 {
		log.Debugf("Runner[%v] statLogPath %v find matches: %v", r.meta.RunnerName, r.logPathPattern, strings.Join(matches, ", "))
	}

	var unmatchMap = make(map[string]bool)
	if r.ignoreLogPathPattern != "" {
		unmatches, err := filepath.Glob(r.ignoreLogPathPattern)
		if err != nil {
			log.Errorf("Runner[%v] stat ignoreLogPathPattern error %v", r.meta.RunnerName, err)
			r.setStatsError("Runner[" + r.meta.RunnerName + "] stat ignoreLogPathPattern error " + err.Error())
			return
		}
		for _, unmatch := range unmatches {
			unmatchMap[unmatch] = true
		}
		if len(unmatches) > 0 {
			log.Debugf("Runner[%v] %d unmatches found after stated ignore log path %q: %v", r.meta.RunnerName, len(unmatches), r.ignoreLogPathPattern, unmatches)
		}
	}
	var newaddsPath []string
	now := time.Now()
	for _, mc := range matches {
		if unmatchMap[mc] {
			continue
		}
		rp, fi, err := GetRealPath(mc)
		if err != nil {
			if !IsSelfRunner(r.meta.RunnerName) {
				log.Errorf("Runner[%v] file pattern %v match %v stat error %v, ignore this match...", r.meta.RunnerName, r.logPathPattern, mc, err)
			} else {
				log.Debugf("Runner[%v] file pattern %v match %v stat error %v, ignore this match...", r.meta.RunnerName, r.logPathPattern, mc, err)
			}
			continue
		}
		if fi.IsDir() {
			log.Debugf("Runner[%v] %v is dir, mode[tailx] only support read file, ignore this match...", r.meta.RunnerName, mc)
			continue
		}
		r.armapmux.Lock()
		filear, ok := r.fileReaders[rp]
		r.armapmux.Unlock()
		if ok {
			if IsFileModified(rp, r.statInterval, now) {
				filear.Start()
			}
			log.Debugf("Runner[%v] <%v> is collecting, ignore...", r.meta.RunnerName, rp)
			continue
		}
		r.armapmux.Lock()
		cacheline := r.cacheMap[rp]
		r.armapmux.Unlock()
		// 过期的文件不追踪，除非之前追踪的并且有日志没读完
		// 如果过期时间为 0，则永不过期
		if cacheline == "" &&
			r.expire.Nanoseconds() > 0 && fi.ModTime().Add(r.expire).Before(time.Now()) {
			log.Debugf("Runner[%v] <%v> is expired, ignore...", r.meta.RunnerName, mc)
			continue
		}
		ar, err := NewActiveReader(mc, rp, r.whence, r.notFirstTime, r.meta, r.msgChan, r.errChan)
		if err != nil {
			err = fmt.Errorf("runner[%v] NewActiveReader for matches %v error %v", r.meta.RunnerName, rp, err)
			r.sendError(err)
			if !IsSelfRunner(r.meta.RunnerName) {
				log.Error(err, ", ignore this match...")
			} else {
				log.Debug(err, ", ignore this match...")
			}
			continue
		}
		ar.readcache = cacheline
		if r.headRegexp != nil {
			err = ar.br.SetMode(ReadModeHeadPatternRegexp, r.headRegexp)
			if err != nil {
				if !IsSelfRunner(r.meta.RunnerName) {
					log.Errorf("Runner[%v] NewActiveReader for matches %v SetMode error %v", r.meta.RunnerName, rp, err)
				} else {
					log.Debugf("Runner[%v] NewActiveReader for matches %v SetMode error %v", r.meta.RunnerName, rp, err)
				}
				r.setStatsError("Runner[" + r.meta.RunnerName + "] NewActiveReader for matches " + rp + " SetMode error " + err.Error())
			}
		}
		newaddsPath = append(newaddsPath, rp)
		r.armapmux.Lock()
		if !r.hasStopped() && !r.isStopping() {
			if err = r.meta.AddSubMeta(rp, ar.br.Meta); err != nil {
				if !IsSelfRunner(r.meta.RunnerName) {
					log.Errorf("Runner[%v] %v add submeta for %v err %v, but this reader will still working", r.meta.RunnerName, mc, rp, err)
				} else {
					log.Debugf("Runner[%v] %v add submeta for %v err %v, but this reader will still working", r.meta.RunnerName, mc, rp, err)
				}
			}
			r.fileReaders[rp] = ar
		} else {
			if !IsSelfRunner(r.meta.RunnerName) {
				log.Warnf("Runner[%v] %v NewActiveReader but reader was stopped, ignore this...", r.meta.RunnerName, mc)
			} else {
				log.Debugf("Runner[%v] %v NewActiveReader but reader was stopped, ignore this...", r.meta.RunnerName, mc)
			}
		}
		r.armapmux.Unlock()
		if !r.hasStopped() && !r.isStopping() {
			ar.Start()
		} else {
			if !IsSelfRunner(r.meta.RunnerName) {
				log.Warnf("Runner[%v] %v NewActiveReader but reader was stopped, will not running...", r.meta.RunnerName, mc)
			} else {
				log.Debugf("Runner[%v] %v NewActiveReader but reader was stopped, will not running...", r.meta.RunnerName, mc)
			}
		}
	}
	if !r.notFirstTime {
		r.notFirstTime = true
	}
	if len(newaddsPath) > 0 {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Infof("Runner[%v] statLogPath find new logpath: %v", r.meta.RunnerName, strings.Join(newaddsPath, ", "))
		} else {
			log.Debugf("Runner[%v] statLogPath find new logpath: %v", r.Name(), strings.Join(newaddsPath, ", "))
		}
	}
}

func (r *Reader) Start() error {
	if r.isStopping() || r.hasStopped() {
		return errors.New("reader is stopping or has stopped")
	} else if !atomic.CompareAndSwapInt32(&r.status, StatusInit, StatusRunning) {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Warnf("Runner[%v] %q daemon has already started and is running", r.meta.RunnerName, r.Name())
		} else {
			log.Debugf("Runner[%v] %q daemon has already started and is running", r.meta.RunnerName, r.Name())
		}
		return nil
	}

	go func() {
		ticker := time.NewTicker(r.statInterval)
		defer ticker.Stop()
		for {
			r.checkExpiredFiles()
			r.statLogPath()

			select {
			case <-r.stopChan:
				atomic.StoreInt32(&r.status, StatusStopped)
				if !IsSelfRunner(r.meta.RunnerName) {
					log.Infof("Runner[%v] %q daemon has stopped from running", r.meta.RunnerName, r.Name())
				} else {
					log.Debugf("Runner[%v] %q daemon has stopped from running", r.meta.RunnerName, r.Name())
				}
				return
			case <-ticker.C:
			}
		}
	}()

	if IsSubMetaExpire(r.submetaExpire, r.expire) {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				r.meta.CheckExpiredSubMetas(r.submetaExpire)

				select {
				case <-r.stopChan:
					return
				case <-ticker.C:
				}
			}
		}()
	}

	if !IsSelfRunner(r.meta.RunnerName) {
		log.Infof("Runner[%v] %q daemon has started", r.meta.RunnerName, r.Name())
	} else {
		log.Debugf("Runner[%v] %q daemon has started", r.meta.RunnerName, r.Name())
	}
	return nil
}

func (r *Reader) getActiveReaders() []*ActiveReader {
	r.armapmux.Lock()
	defer r.armapmux.Unlock()
	var ars []*ActiveReader
	for _, ar := range r.fileReaders {
		ars = append(ars, ar)
	}
	return ars
}

func (r *Reader) Source() string {
	return r.currentFile
}

// Note: 对 currentFile 的操作非线程安全，需由上层逻辑保证同步调用 ReadLine
func (r *Reader) ReadLine() (string, error) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case msg := <-r.msgChan:
		r.currentFile = msg.logpath
		return msg.result, nil
	case err := <-r.errChan:
		return "", err
	case <-timer.C:
	}

	return "", nil
}

func (r *Reader) Status() StatsInfo {
	r.statsLock.RLock()
	defer r.statsLock.RUnlock()

	ars := r.getActiveReaders()
	for _, ar := range ars {
		st := ar.Status()
		if st.LastError != "" {
			r.stats.LastError += "\n<" + ar.originpath + ">: " + st.LastError
		}
	}
	return r.stats
}

func (r *Reader) Lag() (*LagInfo, error) {
	lagInfo := &LagInfo{SizeUnit: "bytes"}
	var errStr string
	ars := r.getActiveReaders()

	for _, ar := range ars {
		lg, subErr := ar.Lag()
		if subErr != nil {
			errStr += subErr.Error()
			log.Warn(subErr)
			continue
		}
		lagInfo.Size += lg.Size
	}

	var err error
	if len(errStr) > 0 {
		err = errors.New(errStr)
	}
	return lagInfo, err
}

// SyncMeta 从队列取数据时同步队列，作用在于保证数据不重复
func (r *Reader) SyncMeta() {
	ars := r.getActiveReaders()
	for _, ar := range ars {
		readcache := ar.SyncMeta()
		if readcache == "" {
			continue
		}
		r.armapmux.Lock()
		r.cacheMap[ar.realpath] = readcache
		r.armapmux.Unlock()
	}
	r.armapmux.Lock()
	buf, err := jsoniter.Marshal(r.cacheMap)
	r.armapmux.Unlock()
	if err != nil {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Errorf("%s sync meta error %v, cacheMap %v", r.Name(), err, r.cacheMap)
		} else {
			log.Debugf("Runner[%s] %s sync meta error %v, cacheMap %v", r.meta.RunnerName, r.Name(), err, r.cacheMap)
		}
		return
	}
	err = r.meta.WriteBuf(buf, 0, 0, len(buf))
	if err != nil {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Errorf("%v sync meta WriteBuf error %v, buf %v", r.Name(), err, string(buf))
		} else {
			log.Debugf("Runner[%s] %s sync meta WriteBuf error %v, buf %v", r.meta.RunnerName, r.Name(), err, string(buf))
		}
		return
	}

	if IsSubMetaExpire(r.submetaExpire, r.expire) {
		r.meta.CleanExpiredSubMetas(r.submetaExpire)
	}
}

func (r *Reader) Close() error {
	if !atomic.CompareAndSwapInt32(&r.status, StatusRunning, StatusStopping) {
		if !IsSelfRunner(r.meta.RunnerName) {
			log.Warnf("Runner[%v] reader %q is not running, close operation ignored", r.meta.RunnerName, r.Name())
		} else {
			log.Debugf("Runner[%v] reader %q is not running, close operation ignored", r.meta.RunnerName, r.Name())
		}
		return nil
	}
	log.Debugf("Runner[%v] %q daemon is stopping", r.meta.RunnerName, r.Name())
	close(r.stopChan)

	// 停10ms为了管道中的数据传递完毕，确认reader run函数已经结束不会再读取，保证syncMeta的正确性
	time.Sleep(10 * time.Millisecond)
	r.SyncMeta()
	ars := r.getActiveReaders()
	var wg sync.WaitGroup
	for _, ar := range ars {
		wg.Add(1)
		go func(mar *ActiveReader) {
			defer wg.Done()
			xerr := mar.Close()
			if xerr != nil {
				if !IsSelfRunner(r.meta.RunnerName) {
					log.Errorf("Runner[%v] Close ActiveReader %v error %v", r.meta.RunnerName, mar.originpath, xerr)
				} else {
					log.Debugf("Runner[%v] Close ActiveReader %v error %v", r.meta.RunnerName, mar.originpath, xerr)
				}
			}
		}(ar)
	}
	wg.Wait()

	// 在所有 active readers 关闭完成后再关闭管道
	close(r.msgChan)
	close(r.errChan)
	return nil
}

func (r *Reader) Reset() error {
	errMsg := make([]string, 0)
	if err := r.meta.Reset(); err != nil {
		errMsg = append(errMsg, err.Error())
	}
	ars := r.getActiveReaders()
	for _, ar := range ars {
		if ar.br != nil {
			if subErr := ar.br.Meta.Reset(); subErr != nil {
				errMsg = append(errMsg, subErr.Error())
			}
		}
	}
	if len(errMsg) != 0 {
		return errors.New(strings.Join(errMsg, "\n"))
	}
	return nil
}
