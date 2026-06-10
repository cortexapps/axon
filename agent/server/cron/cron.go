package cron

import (
	"strconv"

	cronLib "github.com/robfig/cron/v3"
)

type Cron interface {
	Add(spec string, cmd func()) (string, error)
	Remove(id string)
}

func New() Cron {
	c := cronLib.New()
	// The robfig/cron scheduler only dispatches jobs once it has been
	// started; without this, AddFunc registers entries that never fire.
	c.Start()
	return &cronLibWrapper{
		cron: c,
	}
}

type cronLibWrapper struct {
	cron *cronLib.Cron
}

func (c *cronLibWrapper) Add(spec string, cmd func()) (string, error) {
	res, err := c.cron.AddFunc(spec, cmd)
	if err != nil {
		return "", err
	}

	id := strconv.Itoa(int(res))

	return id, nil
}

func (c *cronLibWrapper) Remove(id string) {
	cronId, err := strconv.Atoi(id)
	if err != nil {
		panic(err)
	}
	c.cron.Remove(cronLib.EntryID(cronId))
}

type noopCron struct {
}

func NewNoopCron() Cron {
	return &noopCron{}
}

func (c *noopCron) Add(spec string, cmd func()) (string, error) {

	return "", nil
}

func (c *noopCron) Remove(id string) {

}
