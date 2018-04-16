// Copyright 2013-2015 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package replicaset

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	stdtesting "testing"
	"time"

	"github.com/juju/errors"
	"github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils"
	gc "gopkg.in/check.v1"
	"gopkg.in/mgo.v2"
)

const rsName = "juju"

func TestPackage(t *stdtesting.T) {
	gc.TestingT(t)
}

type MongoSuite struct {
	testing.IsolationSuite
	root *testing.MgoInstance
}

func newServer(c *gc.C) *testing.MgoInstance {
	inst := &testing.MgoInstance{Params: []string{"--replSet", rsName}}
	err := inst.Start(nil)
	c.Assert(err, jc.ErrorIsNil)

	session, err := inst.DialDirect()
	if err != nil {
		inst.Destroy()
		c.Fatalf("error dialing mongo server: %v", err.Error())
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)
	if err = session.Ping(); err != nil {
		inst.Destroy()
		c.Fatalf("error pinging mongo server: %v", err.Error())
	}
	return inst
}

var _ = gc.Suite(&MongoSuite{})

func (s *MongoSuite) SetUpTest(c *gc.C) {
	s.IsolationSuite.SetUpTest(c)
	s.root = newServer(c)
	dialAndTestInitiate(c, s.root, s.root.Addr())
}

func (s *MongoSuite) TearDownTest(c *gc.C) {
	s.root.Destroy()
	s.IsolationSuite.TearDownTest(c)
}

var initialTags = map[string]string{"foo": "bar"}

// assertMembers asserts the known field values of a retrieved and expected
// Members slice are equal.
func assertMembers(c *gc.C, mems []Member, expectedMembers []Member) {
	// 2.x and 3.2 seem to use different default values for bool. For
	// example, in 3.2 false is false, not nil, and we can't know the
	// pointer value to check with DeepEquals.
	for i, _ := range mems {
		c.Assert(mems[i].Id, gc.Equals, expectedMembers[i].Id)
		c.Assert(mems[i].Address, gc.Equals, expectedMembers[i].Address)
		c.Assert(mems[i].Tags, jc.DeepEquals, expectedMembers[i].Tags)
	}
}

func dialAndTestInitiate(c *gc.C, inst *testing.MgoInstance, addr string) {
	session := inst.MustDialDirect()
	defer session.Close()

	mode := session.Mode()
	err := Initiate(session, addr, rsName, initialTags)
	c.Assert(err, jc.ErrorIsNil)

	// make sure we haven't messed with the session's mode
	c.Assert(session.Mode(), gc.Equals, mode)

	// Ids start at 1 for us, so we can differentiate between set and unset
	expectedMembers := []Member{{Id: 1, Address: addr, Tags: initialTags}}

	// need to set mode to strong so that we wait for the write to succeed
	// before reading and thus ensure that we're getting consistent reads.
	session.SetMode(mgo.Strong, false)

	mems, err := CurrentMembers(session)
	c.Assert(err, jc.ErrorIsNil)
	assertMembers(c, mems, expectedMembers)

	loadData(session, c)
}

func (s *MongoSuite) TestInitiateWaitsForStatus(c *gc.C) {
	s.root.Destroy()

	// create a new server that hasn't been initiated
	s.root = newServer(c)
	session := s.root.MustDialDirect()
	defer session.Close()

	i := 0
	mockStatus := func(session *mgo.Session) (*Status, error) {
		status := &Status{}
		var err error
		i += 1
		if i < 20 {
			err = fmt.Errorf("bang!")
		} else if i > 20 {
			// when i == 20 then len(status.Members) == 0
			// so we will be called one more time until we populate
			// Members
			status.Members = append(status.Members, MemberStatus{Id: 1})
		}
		return status, err
	}

	s.PatchValue(&getCurrentStatus, mockStatus)
	Initiate(session, s.root.Addr(), rsName, initialTags)
	c.Assert(i, gc.Equals, 21)
}

func loadData(session *mgo.Session, c *gc.C) {
	type foo struct {
		Name    string
		Address string
		Count   int
	}

	for col := 0; col < 10; col++ {
		// Testing with mongodb3.2 showed the need to make foos a slice
		// if interface{} (Insert expects a slice not an empty
		// interface) passed in expanded. Passing a slice of foo to
		// Insert gives this error, with the slice perhaps not handled
		// by writeOp().
		// `Message:"error parsing element 0 of field documents :: caused by :: wrong type for '0' field, expected object`
		foos := make([]interface{}, 10000)
		for n := range foos {
			foos[n] = foo{
				Name:    fmt.Sprintf("name_%d_%d", col, n),
				Address: fmt.Sprintf("address_%d_%d", col, n),
				Count:   n * (col + 1),
			}
		}

		err := session.DB("testing").C(fmt.Sprintf("data%d", col)).Insert(foos...)
		c.Assert(err, jc.ErrorIsNil)
	}
}

func attemptLoop(c *gc.C, strategy utils.AttemptStrategy, desc string, f func() error) {
	var err error
	start := time.Now()
	attemptCount := 0
	for attempt := strategy.Start(); attempt.Next(); {
		attemptCount += 1
		if err = f(); err == nil || !attempt.HasNext() {
			break
		}
		c.Logf("%s failed: %v", desc, err)
	}
	c.Logf("%s: %d attempts in %s", desc, attemptCount, time.Since(start))
	c.Assert(err, jc.ErrorIsNil)
}

func (s *MongoSuite) TestAddRemoveSet(c *gc.C) {
	if runtime.GOARCH == "386" {
		c.Skip(fmt.Sprintf("Test disabled on i386 until fixed - see bug lp:1425569"))
	}
	getAddr := func(inst *testing.MgoInstance) string {
		return inst.Addr()
	}
	assertAddRemoveSet(c, s.root, getAddr)
}

func assertAddRemoveSet(c *gc.C, root *testing.MgoInstance, getAddr func(*testing.MgoInstance) string) {
	session := root.MustDial()
	defer session.Close()

	members := make([]Member, 0, 5)

	// Add should be idempotent, so re-adding root here shouldn't result in
	// two copies of root in the replica set
	members = append(members, Member{Address: getAddr(root), Tags: initialTags})

	// We allow for up to 2 minutes  per operation, since Add, Set, etc. call
	// replSetReconfig which may cause primary renegotiation. According
	// to the Mongo docs, "typically this is 10-20 seconds, but could be
	// as long as a minute or more."
	//
	// Note that the delay is set at 500ms to cater for relatively quick
	// operations without thrashing on those that take longer.
	strategy := utils.AttemptStrategy{Total: time.Minute * 2, Delay: time.Millisecond * 500}

	instances := make([]*testing.MgoInstance, 5)
	instances[0] = root
	for i := 1; i < len(instances); i++ {
		inst := newServer(c)
		instances[i] = inst
		// no need to Remove the instances from the replicaset as
		// we're destroying the replica set immediately afterwards
		defer inst.Destroy()
		key := fmt.Sprintf("key%d", i)
		val := fmt.Sprintf("val%d", i)
		tags := map[string]string{key: val}
		members = append(members, Member{Address: getAddr(inst), Tags: tags})
	}

	attemptLoop(c, strategy, "Add()", func() error {
		return Add(session, members...)
	})

	expectedMembers := make([]Member, len(members))
	for i, m := range members {
		// Ids should start at 1 (for the root) and go up
		m.Id = i + 1
		expectedMembers[i] = m
	}

	var cfg *Config
	attemptLoop(c, strategy, "CurrentConfig()", func() error {
		var err error
		cfg, err = CurrentConfig(session)
		return err
	})
	c.Assert(cfg.Name, gc.Equals, rsName)
	// 2 since we already changed it once
	c.Assert(cfg.Version, gc.Equals, 2)

	mems := cfg.Members
	assertMembers(c, mems, expectedMembers)

	// Now remove the last two Members...
	attemptLoop(c, strategy, "Remove()", func() error {
		return Remove(session, members[3].Address, members[4].Address)
	})
	expectedMembers = expectedMembers[0:3]

	// ... and confirm that CurrentMembers reflects the removal.
	attemptLoop(c, strategy, "CurrentMembers()", func() error {
		var err error
		mems, err = CurrentMembers(session)
		return err
	})
	assertMembers(c, mems, expectedMembers)

	// now let's mix it up and set the new members to a mix of the previous
	// plus the new arbiter
	// Also have an explicitly large member Id to make sure we don't have possible
	// collisions
	mem4 := members[4]
	mem4.Id = 10
	mems = []Member{members[3], mems[2], mems[0], mem4}
	attemptLoop(c, strategy, "Set()", func() error {
		err := Set(session, mems)
		if err != nil {
			c.Logf("current session mode: %v", session.Mode())
			session.Refresh()
		}
		return err
	})

	attemptLoop(c, strategy, "Ping()", func() error {
		// can dial whichever replica address here, mongo will figure it out
		if session != nil {
			session.Close()
		}
		session = instances[0].MustDialDirect()
		return session.Ping()
	})

	// any new members will get an id of max(other_ids...)+1
	expectedMembers = []Member{members[3], expectedMembers[2], expectedMembers[0], members[4]}
	expectedMembers[0].Id = 11
	expectedMembers[3].Id = 10

	attemptLoop(c, strategy, "CurrentMembers()", func() error {
		var err error
		mems, err = CurrentMembers(session)
		return err
	})
	assertMembers(c, mems, expectedMembers)
}

func (s *MongoSuite) TestIsMaster(c *gc.C) {
	session := s.root.MustDial()
	defer session.Close()

	expected := IsMasterResults{
		// The following fields hold information about the specific mongodb node.
		IsMaster:  true,
		Secondary: false,
		Arbiter:   false,
		Address:   s.root.Addr(),
		LocalTime: time.Time{},

		// The following fields hold information about the replica set.
		ReplicaSetName: rsName,
		Addresses:      []string{s.root.Addr()},
		Arbiters:       nil,
		PrimaryAddress: s.root.Addr(),
	}

	res, err := IsMaster(session)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(closeEnough(res.LocalTime, time.Now()), jc.IsTrue)
	res.LocalTime = time.Time{}
	c.Check(*res, jc.DeepEquals, expected)
}

func (s *MongoSuite) TestMasterHostPort(c *gc.C) {
	session := s.root.MustDial()
	defer session.Close()

	expected := s.root.Addr()
	result, err := MasterHostPort(session)

	c.Logf("TestMasterHostPort expected: %v, got: %v", expected, result)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(result, gc.Equals, expected)
}

func (s *MongoSuite) TestMasterHostPortOnUnconfiguredReplicaSet(c *gc.C) {
	inst := &testing.MgoInstance{}
	err := inst.Start(nil)
	c.Assert(err, jc.ErrorIsNil)
	defer inst.Destroy()
	session := inst.MustDial()
	hp, err := MasterHostPort(session)
	c.Assert(err, gc.Equals, ErrMasterNotConfigured)
	c.Assert(hp, gc.Equals, "")
}

func (s *MongoSuite) TestIsReadyOne(c *gc.C) {
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) {
			status := &Status{Members: []MemberStatus{{
				Id:      1,
				Healthy: true,
			}}}
			return status, nil
		},
	)
	session := s.root.MustDial()
	defer session.Close()

	ready, err := IsReady(session)
	c.Assert(err, jc.ErrorIsNil)

	c.Check(ready, jc.IsTrue)
}

func (s *MongoSuite) TestIsReadyMultiple(c *gc.C) {
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) {
			status := &Status{}
			for i := 1; i < 5; i++ {
				member := MemberStatus{Id: i + 1, Healthy: true}
				status.Members = append(status.Members, member)
			}
			return status, nil
		},
	)
	session := s.root.MustDial()
	defer session.Close()

	ready, err := IsReady(session)
	c.Assert(err, jc.ErrorIsNil)

	c.Check(ready, jc.IsTrue)
}

func (s *MongoSuite) TestIsReadyNotOne(c *gc.C) {
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) {
			status := &Status{Members: []MemberStatus{{
				Id:      1,
				Healthy: false,
			}}}
			return status, nil
		},
	)
	session := s.root.MustDial()
	defer session.Close()

	ready, err := IsReady(session)
	c.Assert(err, jc.ErrorIsNil)

	c.Check(ready, jc.IsFalse)
}

func (s *MongoSuite) TestIsReadyMinority(c *gc.C) {
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) {
			status := &Status{Members: []MemberStatus{{
				Id:      1,
				Healthy: true,
			},
				{
					Id:      2,
					Healthy: false,
				},
				{
					Id:      3,
					Healthy: false,
				}}}
			return status, nil
		},
	)
	session := s.root.MustDial()
	defer session.Close()

	ready, err := IsReady(session)
	c.Assert(err, jc.ErrorIsNil)

	c.Check(ready, jc.IsFalse)
}

func (s *MongoSuite) checkConnectionFailure(c *gc.C, failure error) {
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) { return nil, failure },
	)
	session := s.root.MustDial()
	defer session.Close()

	ready, err := IsReady(session)
	c.Assert(err, jc.ErrorIsNil)

	c.Check(ready, jc.IsFalse)
}

func (s *MongoSuite) TestIsReadyConnectionDropped(c *gc.C) {
	s.checkConnectionFailure(c, io.EOF)
}

func (s *MongoSuite) TestIsReadyConnectionFailedWithErrno(c *gc.C) {
	for _, errno := range connectionErrors {
		c.Logf("Checking errno %#v (%v)", errno, errno)
		s.checkConnectionFailure(c, errno)
	}
}

func (s *MongoSuite) TestIsReadyError(c *gc.C) {
	failure := errors.New("failed!")
	s.PatchValue(&getCurrentStatus,
		func(session *mgo.Session) (*Status, error) { return nil, failure },
	)
	session := s.root.MustDial()
	defer session.Close()

	_, err := IsReady(session)
	c.Check(errors.Cause(err), gc.Equals, failure)
}

func (s *MongoSuite) TestWaitUntilReady(c *gc.C) {
	var isReadyCalled bool
	mockIsReady := func(session *mgo.Session) (bool, error) {
		isReadyCalled = true
		return true, nil
	}

	s.PatchValue(&isReady, mockIsReady)
	session := s.root.MustDial()
	defer session.Close()

	err := WaitUntilReady(session, 10)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(isReadyCalled, jc.IsTrue)
}

func (s *MongoSuite) TestWaitUntilReadyTimeout(c *gc.C) {
	mockIsReady := func(session *mgo.Session) (bool, error) {
		return false, nil
	}

	s.PatchValue(&isReady, mockIsReady)
	session := s.root.MustDial()
	defer session.Close()

	err := WaitUntilReady(session, 0)
	c.Assert(err, gc.ErrorMatches, "timed out after 0 seconds")
}

func (s *MongoSuite) TestWaitUntilReadyError(c *gc.C) {
	mockIsReady := func(session *mgo.Session) (bool, error) {
		return false, errors.New("foobar")
	}

	s.PatchValue(&isReady, mockIsReady)
	session := s.root.MustDial()
	defer session.Close()

	err := WaitUntilReady(session, 0)
	c.Assert(err, gc.ErrorMatches, "foobar")
}

func (s *MongoSuite) TestCurrentStatus(c *gc.C) {
	session := s.root.MustDial()
	defer session.Close()

	inst1 := newServer(c)
	defer inst1.Destroy()
	defer Remove(session, inst1.Addr())

	inst2 := newServer(c)
	defer inst2.Destroy()
	defer Remove(session, inst2.Addr())

	var err error
	strategy := utils.AttemptStrategy{Total: time.Minute * 2, Delay: time.Millisecond * 500}
	attempt := strategy.Start()
	for attempt.Next() {
		err = Add(session, Member{Address: inst1.Addr()}, Member{Address: inst2.Addr()})
		if err == nil || !attempt.HasNext() {
			break
		}
	}
	c.Assert(err, jc.ErrorIsNil)

	expected := &Status{
		Name: rsName,
		Members: []MemberStatus{{
			Id:      1,
			Address: s.root.Addr(),
			Self:    true,
			ErrMsg:  "",
			Healthy: true,
			State:   PrimaryState,
		}, {
			Id:      2,
			Address: inst1.Addr(),
			Self:    false,
			ErrMsg:  "",
			Healthy: true,
			State:   SecondaryState,
		}, {
			Id:      3,
			Address: inst2.Addr(),
			Self:    false,
			ErrMsg:  "",
			Healthy: true,
			State:   SecondaryState,
		}},
	}

	strategy.Total = time.Second * 90
	attempt = strategy.Start()
	var res *Status
	for attempt.Next() {
		var err error
		res, err = CurrentStatus(session)
		if err != nil {
			if !attempt.HasNext() {
				c.Errorf("Couldn't get status before timeout, got err: %v", err)
				return
			} else {
				// try again
				continue
			}
		}

		if res.Members[0].State == PrimaryState &&
			res.Members[1].State == SecondaryState &&
			res.Members[2].State == SecondaryState {
			break
		}
		if !attempt.HasNext() {
			c.Errorf("Servers did not get into final state before timeout.  Status: %#v", res)
			return
		}
	}

	for x := range res.Members {
		// non-empty uptime and ping
		c.Check(res.Members[x].Uptime, gc.Not(gc.Equals), 0)

		// ping is always going to be zero since we're on localhost
		// so we can't really test it right now

		// now overwrite Uptime so it won't throw off DeepEquals
		res.Members[x].Uptime = 0
	}
	c.Check(res, jc.DeepEquals, expected)
}

func closeEnough(expected, obtained time.Time) bool {
	t := obtained.Sub(expected)
	return (-500*time.Millisecond) < t && t < (500*time.Millisecond)
}

func findPrimary(c *gc.C, session *mgo.Session) int {
	status, err := CurrentStatus(session)
	c.Assert(err, jc.ErrorIsNil)
	for i, m := range status.Members {
		if m.State == PrimaryState {
			return i
		}
	}
	return -1
}

func (s *MongoSuite) TestStepDownPrimary(c *gc.C) {
	session := s.root.MustDial()
	defer func() {
		if session != nil {
			session.Close()
			session = nil
		}
	}()
	s0 := s.root
	s1 := newServer(c)
	defer s1.Destroy()
	s2 := newServer(c)
	defer s2.Destroy()
	strategy := utils.AttemptStrategy{Total: time.Minute * 2, Delay: time.Millisecond * 500}
	attemptLoop(c, strategy, "Add()", func() error {
		return Add(session, Member{
			Address: s1.Addr(),
		}, Member{
			Address: s2.Addr(),
		})
	})
	mems, err := CurrentMembers(session)
	c.Assert(err, jc.ErrorIsNil)
	assertMembers(c, mems, []Member{{
		Id:      1,
		Address: s0.Addr(),
		Tags:    initialTags,
	}, {
		Id:      2,
		Address: s1.Addr(),
	}, {
		Id:      3,
		Address: s2.Addr(),
	}})
	// find the current primary
	initialPrimary := findPrimary(c, session)
	c.Assert(initialPrimary, jc.GreaterThan, int(-1))
	// ensure the secondaries are up and happy
	// strategy = utils.AttemptStrategy{Total: time.Second, Delay: time.Millisecond * 50}
	attemptLoop(c, strategy, "secondaries are ready", func() error {
		status, err := CurrentStatus(session)
		if err != nil {
			return err
		}
		var notReady []string
		for _, m := range status.Members {
			if m.State != PrimaryState && m.State != SecondaryState {
				notReady = append(notReady, fmt.Sprintf("Member{Id: %d, Address: %s, State: %s}", m.Id, m.Address, m.State.String()))
			}
		}
		if len(notReady) > 0 {
			return errors.Errorf("members not ready: %s", strings.Join(notReady, ", "))
		}
		return nil
	})
	// Now that the secondaries are up, we should be able to ask the primary to step down and notice that the primary changes
	err = StepDownPrimary(session)
	c.Assert(err, jc.ErrorIsNil)
	// Changing the primary should cause us to get disconnected, so we need to reconnect
	session.Close()
	session = nil
	attemptLoop(c, strategy, "reconnect", func() error {
		session, err = s.root.Dial()
		if err != nil {
			session = nil
			return err
		}
		return nil
	})
	// Now that we are reconnected, we should definitely have a different primary
	newPrimary := findPrimary(c, session)
	c.Check(newPrimary, gc.Not(gc.Equals), initialPrimary)
}

func ipv6GetAddr(inst *testing.MgoInstance) string {
	return fmt.Sprintf("[::1]:%v", inst.Port())
}

type MongoIPV6Suite struct {
	testing.IsolationSuite
}

var _ = gc.Suite(&MongoIPV6Suite{})

func (s *MongoIPV6Suite) TestAddRemoveSetIPv6(c *gc.C) {
	root := newServer(c)
	defer root.Destroy()
	dialAndTestInitiate(c, root, ipv6GetAddr(root))
	assertAddRemoveSet(c, root, ipv6GetAddr)
}

func (s *MongoIPV6Suite) TestAddressFixing(c *gc.C) {
	root := newServer(c)
	defer root.Destroy()
	dialAndTestInitiate(c, root, ipv6GetAddr(root))
	session := root.MustDial()
	defer session.Close()

	status, err := CurrentStatus(session)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(len(status.Members), jc.DeepEquals, 1)
	c.Check(status.Members[0].Address, gc.Equals, ipv6GetAddr(root))

	cfg, err := CurrentConfig(session)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(len(cfg.Members), jc.DeepEquals, 1)
	c.Check(cfg.Members[0].Address, gc.Equals, ipv6GetAddr(root))

	result, err := IsMaster(session)
	c.Assert(err, jc.ErrorIsNil)
	c.Check(result.Address, gc.Equals, ipv6GetAddr(root))
	c.Check(result.PrimaryAddress, gc.Equals, ipv6GetAddr(root))
	c.Check(result.Addresses, jc.DeepEquals, []string{ipv6GetAddr(root)})
}
