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
	return &cronLibWrapper{
		cron: cronLib.New(),
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
