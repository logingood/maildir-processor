package maildir_processor

import (
	"errors"
	"fmt"
	"github.com/DataDog/datadog-go/statsd"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
	"github.com/flashmob/go-maildir"
	_ "github.com/sloonz/go-maildir"
	"io"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

const MailDirFilePerms = 0600

type maildirConfig struct {
	// maildir_path may contain a [user] placeholder. This will be substituted at run time
	// eg /home/[user]/Maildir will get substituted to /home/test/Maildir for test@example.com
	Path string `json:"maildir_path"`
	// This is a string holding user to group/id mappings - in other words, the recipient table
	// Each record separated by ","
	// Records have the following format: <username>=<id>:<group>
	// use -1 for <id> & <group> if you want to ignore these, otherwise get these numbers from /etc/passwd
	// Example: "test=1002:2003,guerrilla=1001:1001"
	UserMap string `json:"maildir_user_map"`
}

type MailDir struct {
	userMap map[string][]int
	dirs    map[string]*maildir.Maildir
	config  *maildirConfig
}

func sendToDD(metric string, tags []string) {
	c, _ := statsd.New("127.0.0.1:8125")
	c.Namespace = "guerrillad."
	c.Tags = tags

	_ = c.Incr(metric, tags, 1.0)
}

func trainRspamd(buf io.Reader, rspamdFuzzyEndpoint string, rspamdFuzzyPassword string) error {
	req, err := http.NewRequest("POST", rspamdFuzzyEndpoint, buf)
	req.Header.Add("Password", rspamdFuzzyPassword)
	req.Header.Add("Flag", `11`)
	req.Header.Add("Weight", `10`)

	client := &http.Client{}
	resp, err := client.Do(req)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		err := errors.New(fmt.Sprintf("Rspamd training error - %s: %s", resp.Body, resp.Status))
		sendToDD("fuzzy.error", []string{})
		return err
	}

	backends.Log().Info("Fuzzy storage has been trained succesfully: ", resp.Status)
	sendToDD("fuzzy.trained", []string{})

	return nil
}

// check to see if we have configured
func (m *MailDir) checkUsers(rcpt []mail.Address, mailDirs map[string]*maildir.Maildir) bool {
	for i := range rcpt {
		if _, ok := mailDirs[rcpt[i].User]; !ok {
			return false
		}
	}
	return true
}

var mdirMux sync.Mutex

// initDirs creates the mail dir folders if they haven't been created already
func (m *MailDir) initDirs() error {
	if m.dirs == nil {
		m.dirs = make(map[string]*maildir.Maildir, 0)
	}
	// initialize some maildirs
	mdirMux.Lock()
	defer mdirMux.Unlock()
	for str, ids := range m.userMap {
		path := strings.Replace(m.config.Path, "[user]", str, 1)
		if mdir, err := maildir.NewWithPerm(path, true, MailDirFilePerms, ids[0], ids[1]); err == nil {
			m.dirs[str] = mdir
		} else {
			backends.Log().WithError(err).Error("could not create Maildir. Please check the config")
			return err
		}
	}
	return nil
}

func (m *MailDir) validateRcpt(addr *mail.Address) backends.RcptError {
	u := strings.ToLower(addr.User)
	mdir, ok := m.dirs[u]
	if !ok {
		return backends.NoSuchUser
	}
	if _, err := os.Stat(mdir.Path); err != nil {
		return backends.StorageNotAvailable
	}
	return nil
}

func newMailDir(config *maildirConfig) (*MailDir, error) {
	m := &MailDir{}
	m.config = config
	m.userMap = usermap(m.config.UserMap)
	if strings.Index(m.config.Path, "~/") == 0 {
		// expand the ~/ to home dir
		usr, err := user.Current()
		if err != nil {
			backends.Log().WithError(err).Error("could not expand ~/ to homedir")
			return nil, err
		}
		m.config.Path = usr.HomeDir + m.config.Path[1:]
	}
	if err := m.initDirs(); err != nil {
		return nil, err
	}
	return m, nil
}

// usermap parses the usermap config strings and returns the result in a map
// Example: "test=1002:2003,guerrilla=1001:1001"
// test and guerrilla are usernames
// number 1002 is the uid, 2003 is gid
func usermap(usermap string) (ret map[string][]int) {
	ret = make(map[string][]int, 0)
	users := strings.Split(usermap, ",")
	for i := range users {
		u := strings.Split(users[i], "=")
		if len(u) != 2 {
			return
		}
		ids := strings.Split(u[1], ":")
		if len(ids) != 2 {
			return
		}
		n := make([]int, 0)
		ret[u[0]] = n
		for k := range ids {
			s, _ := strconv.Atoi(ids[k])
			ret[u[0]] = append(ret[u[0]], s)
		}
	}
	return
}

var Processor = func() backends.Decorator {

	// The following initialization is run when the program first starts

	// config will be populated by the initFunc
	var (
		m *MailDir
	)
	// initFunc is an initializer function which is called when our processor gets created.
	// It gets called for every worker
	initializer := backends.InitializeWith(func(backendConfig backends.BackendConfig) error {
		configType := backends.BaseConfig(&maildirConfig{})
		bcfg, err := backends.Svc.ExtractConfig(backendConfig, configType)

		if err != nil {
			return err
		}
		c := bcfg.(*maildirConfig)
		m, err = newMailDir(c)
		if err != nil {
			return err
		}
		return nil
	})
	// register our initializer
	backends.Svc.AddInitializer(initializer)

	return func(c backends.Processor) backends.Processor {
		// The function will be called on each email transaction.
		// On success, it forwards to the next step in the processor call-stack,
		// or returns with an error if failed
		return backends.ProcessWith(func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {
			if task == backends.TaskValidateRcpt {
				// Check the recipients for each RCPT command.
				// This is called each time a recipient is added,
				// validate only the _last_ recipient that was appended
				if size := len(e.RcptTo); size > 0 {
					if err := m.validateRcpt(&e.RcptTo[size-1]); err != nil {
						backends.Log().WithError(backends.NoSuchUser).Info("recipient not configured: ", e.RcptTo[size-1].User)
						return backends.NewResult(response.Canned.FailRcptCmd), backends.NoSuchUser
					}

				}
				return c.Process(e, task)
			} else if task == backends.TaskSaveMail {
				for i := range e.RcptTo {
					u := strings.ToLower(e.RcptTo[i].User)
					mdir, ok := m.dirs[u]
					if !ok {
						// no such user
						continue
					}

					rdr := e.NewReader()

					err := trainRspamd(rdr, os.Getenv("RSPAMD_FUZZY_URL"), os.Getenv("RSPAMD_PASSWORD"))
					if err != nil {
						backends.Log().WithError(err).Error("Could train rspamd")
						return backends.NewResult(fmt.Sprintf("554 Error: could not train rspamd for [%s]", u)), err
					} else {
						backends.Log().Info("Trained rspamd using email", e.RcptTo[i].User)
					}

					if filename, err := mdir.CreateMail(rdr); err != nil {
						backends.Log().WithError(err).Error("Could not save email")
						return backends.NewResult(fmt.Sprintf("554 Error: could not save email for [%s]", u)), err
					} else {
						tags := []string{fmt.Sprintf("in_rcpt:%s", e.RcptTo[i].User)}
						sendToDD("delivered", tags)
						backends.Log().Debug("saved email as", filename)
					}
				}
				// continue to the next Processor in the decorator chain
				return c.Process(e, task)
			} else {
				return c.Process(e, task)
			}

		})
	}
}
