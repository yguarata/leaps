/*
Copyright (c) 2014 Ashley Jeffs

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package lib

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync"
	"testing"
	"time"

	"github.com/jeffail/leaps/lib/store"
	"github.com/jeffail/leaps/lib/util"
)

/*
testStore - Just stores documents in a map.
*/
type testStore struct {
	documents map[string]store.Document
	mutex     sync.RWMutex
}

/*
Create - Store document in memory.
*/
func (s *testStore) Create(doc store.Document) error {
	return s.Update(doc)
}

/*
Update - Store document in memory.
*/
func (s *testStore) Update(doc store.Document) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.documents[doc.ID] = doc
	return nil
}

/*
Read - Fetch document from memory.
*/
func (s *testStore) Read(id string) (store.Document, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	doc, ok := s.documents[id]
	if !ok {
		return doc, store.ErrDocumentNotExist
	}
	return doc, nil
}

func TestGracefullShutdown(t *testing.T) {
	errChan := make(chan BinderError, 10)

	logger, stats := loggerAndStats()
	doc, _ := store.NewDocument("hello world")

	store := testStore{documents: map[string]store.Document{
		"KILL_ME": *doc,
	}}

	binder, err := NewBinder("KILL_ME", &store, DefaultBinderConfig(), errChan, logger, stats)
	if err != nil {
		t.Errorf("Error: %v", err)
		return
	}

	testClient := binder.Subscribe("")
	delete(store.documents, "KILL_ME")
	testClient.SendTransform(OTransform{Position: 0, Insert: "hello", Version: 2}, time.Second)

	<-errChan
	binder.Close()
}

func TestClientAdminTasks(t *testing.T) {
	errChan := make(chan BinderError, 10)

	logger, stats := loggerAndStats()
	doc, _ := store.NewDocument("hello world")

	store := testStore{documents: map[string]store.Document{
		"KILL_ME": *doc,
	}}

	binder, err := NewBinder("KILL_ME", &store, DefaultBinderConfig(), errChan, logger, stats)
	if err != nil {
		t.Errorf("Error: %v", err)
		return
	}

	nClients := 10

	portals := make([]BinderPortal, nClients)
	clientIDs := make([]string, nClients)

	for i := 0; i < nClients; i++ {
		clientIDs[i] = util.GenerateStampedUUID()
		portals[i] = binder.Subscribe(clientIDs[i])
		if portals[i].Error != nil {
			t.Errorf("Subscribe error: %v\n", portals[i].Error)
			return
		}
	}

	for i := 0; i < nClients; i++ {
		remainingClients, err := binder.GetUsers(time.Second)
		if err != nil {
			t.Errorf("Get users error: %v\n", err)
			return
		}
		if len(remainingClients) != len(clientIDs) {
			t.Errorf("Wrong number of remaining clients: %v != %v\n", len(remainingClients), len(clientIDs))
			return
		}
		for _, val := range clientIDs {
			found := false
			for _, c := range remainingClients {
				if val == c {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Client not found in binder: %v\n", val)
				return
			}
		}

		killID := clientIDs[0]
		clientIDs = clientIDs[1:]

		if err := binder.KickUser(killID, time.Second); err != nil {
			t.Errorf("Kick user error: %v\n", err)
			return
		}
	}

	binder.Close()
}

func TestUpdates(t *testing.T) {
	errChan := make(chan BinderError)
	doc, _ := store.NewDocument("hello world")
	logger, stats := loggerAndStats()

	binder, err := NewBinder(
		doc.ID,
		&testStore{documents: map[string]store.Document{doc.ID: *doc}},
		DefaultBinderConfig(),
		errChan,
		logger,
		stats,
	)
	if err != nil {
		t.Errorf("error: %v", err)
		return
	}

	go func() {
		for e := range errChan {
			t.Errorf("From error channel: %v", e.Err)
		}
	}()

	userID1, userID2 := util.GenerateStampedUUID(), util.GenerateStampedUUID()

	portal1, portal2 := binder.Subscribe(userID1), binder.Subscribe(userID2)

	if userID1 != portal1.Client.UserID {
		t.Errorf("Binder portal wrong user ID: %v != %v", userID1, portal1.Client.UserID)
	}
	if userID2 != portal2.Client.UserID {
		t.Errorf("Binder portal wrong user ID: %v != %v", userID2, portal2.Client.UserID)
	}

	for i := 0; i < 100; i++ {
		portal1.SendMessage(Message{})

		message := <-portal2.MessageRcvChan
		if message.Client.UserID != userID1 {
			t.Errorf("Received incorrect user ID: %v != %v", message.Client.UserID, portal1.Client.UserID)
		}
		if message.Client.SessionID != portal1.Client.SessionID {
			t.Errorf("Received incorrect session ID: %v != %v", message.Client.SessionID, portal1.Client.SessionID)
		}

		portal2.SendMessage(Message{})

		message2 := <-portal1.MessageRcvChan
		if message2.Client.UserID != userID2 {
			t.Errorf("Received incorrect user ID: %v != %v", message2.Client.UserID, portal2.Client.UserID)
		}
		if message2.Client.SessionID != portal2.Client.SessionID {
			t.Errorf("Received incorrect session ID: %v != %v", message2.Client.SessionID, portal2.Client.SessionID)
		}
	}
}

func TestUpdatesSameUserID(t *testing.T) {
	errChan := make(chan BinderError)
	doc, _ := store.NewDocument("hello world")
	logger, stats := loggerAndStats()

	binder, err := NewBinder(
		doc.ID,
		&testStore{documents: map[string]store.Document{doc.ID: *doc}},
		DefaultBinderConfig(),
		errChan,
		logger,
		stats,
	)
	if err != nil {
		t.Errorf("error: %v", err)
		return
	}

	go func() {
		for e := range errChan {
			t.Errorf("From error channel: %v", e.Err)
		}
	}()

	userID := util.GenerateStampedUUID()

	portal1, portal2 := binder.Subscribe(userID), binder.Subscribe(userID)

	if userID != portal1.Client.UserID {
		t.Errorf("Binder portal wrong user ID: %v != %v", userID, portal1.Client.UserID)
	}
	if userID != portal2.Client.UserID {
		t.Errorf("Binder portal wrong user ID: %v != %v", userID, portal2.Client.UserID)
	}

	for i := 0; i < 100; i++ {
		portal1.SendMessage(Message{})

		message := <-portal2.MessageRcvChan
		if message.Client.UserID != userID {
			t.Errorf("Received incorrect user ID: %v != %v", message.Client.UserID, portal1.Client.UserID)
		}
		if message.Client.SessionID != portal1.Client.SessionID {
			t.Errorf("Received incorrect session ID: %v != %v", message.Client.SessionID, portal1.Client.SessionID)
		}

		portal2.SendMessage(Message{})

		message2 := <-portal1.MessageRcvChan
		if message2.Client.UserID != userID {
			t.Errorf("Received incorrect token: %v != %v", message2.Client.UserID, portal2.Client.UserID)
		}
		if message2.Client.SessionID != portal2.Client.SessionID {
			t.Errorf("Received incorrect session ID: %v != %v", message2.Client.SessionID, portal2.Client.SessionID)
		}
	}
}

func TestNewBinder(t *testing.T) {
	errChan := make(chan BinderError)
	doc, _ := store.NewDocument("hello world")
	logger, stats := loggerAndStats()

	binder, err := NewBinder(
		doc.ID,
		&testStore{documents: map[string]store.Document{doc.ID: *doc}},
		DefaultBinderConfig(),
		errChan,
		logger,
		stats,
	)
	if err != nil {
		t.Errorf("error: %v", err)
		return
	}

	go func() {
		for err := range errChan {
			t.Errorf("From error channel: %v", err.Err)
		}
	}()

	portal1, portal2 := binder.Subscribe(""), binder.Subscribe("")
	if v, err := portal1.SendTransform(
		OTransform{
			Position: 6,
			Version:  2,
			Delete:   5,
			Insert:   "universe",
		},
		time.Second,
	); v != 2 || err != nil {
		t.Errorf("Send Transform error, v: %v, err: %v", v, err)
	}
	if v, err := portal2.SendTransform(
		OTransform{
			Position: 0,
			Version:  3,
			Delete:   0,
			Insert:   "super ",
		},
		time.Second,
	); v != 3 || err != nil {
		t.Errorf("Send Transform error, v: %v, err: %v", v, err)
	}

	<-portal1.TransformRcvChan
	<-portal2.TransformRcvChan

	portal3 := binder.Subscribe("")
	if exp, rec := "super hello universe", portal3.Document.Content; exp != rec {
		t.Errorf("Wrong content, expected %v, received %v", exp, rec)
	}
}

func TestReadOnlyPortals(t *testing.T) {
	errChan := make(chan BinderError)
	doc, _ := store.NewDocument("hello world")
	logger, stats := loggerAndStats()

	binder, err := NewBinder(
		doc.ID,
		&testStore{documents: map[string]store.Document{doc.ID: *doc}},
		DefaultBinderConfig(),
		errChan,
		logger,
		stats,
	)
	if err != nil {
		t.Errorf("error: %v", err)
		return
	}

	go func() {
		for err := range errChan {
			t.Errorf("From error channel: %v", err.Err)
		}
	}()

	portal1, portal2 := binder.Subscribe(""), binder.Subscribe("")
	portalReadOnly := binder.SubscribeReadOnly("")

	if v, err := portal1.SendTransform(
		OTransform{
			Position: 6,
			Version:  2,
			Delete:   5,
			Insert:   "universe",
		},
		time.Second,
	); v != 2 || err != nil {
		t.Errorf("Send Transform error, v: %v, err: %v", v, err)
	}
	if v, err := portal2.SendTransform(
		OTransform{
			Position: 0,
			Version:  3,
			Delete:   0,
			Insert:   "super ",
		},
		time.Second,
	); v != 3 || err != nil {
		t.Errorf("Send Transform error, v: %v, err: %v", v, err)
	}

	<-portal1.TransformRcvChan
	<-portal2.TransformRcvChan
	<-portalReadOnly.TransformRcvChan

	if _, err := portalReadOnly.SendTransform(OTransform{}, time.Second); err != ErrReadOnlyPortal {
		t.Errorf("Read only portal unexpected result: %v", err)
	}

	portal3 := binder.Subscribe("")
	if exp, rec := "super hello universe", portal3.Document.Content; exp != rec {
		t.Errorf("Wrong content, expected %v, received %v", exp, rec)
	}
}

/*func badClient(b *BinderPortal, t *testing.T, wg *sync.WaitGroup) {
	// Do nothing, LOLOLOLOLOL AHAHAHAHAHAHAHAHAHA! TIME WASTTTTIIINNNGGGG!!!!
	time.Sleep(500 * time.Millisecond)

	// The first transform is free (buffered chan)
	<-b.TransformRcvChan
	_, open := <-b.TransformRcvChan
	if open {
		t.Errorf("Bad client wasn't rejected")
	}
	wg.Done()
}*/

func goodClient(b BinderPortal, expecting int, t *testing.T, wg *sync.WaitGroup) {
	changes := b.Version + 1
	seen := 0
	for tform := range b.TransformRcvChan {
		seen++
		if tform.Insert != fmt.Sprintf("%v", changes) {
			t.Errorf("Wrong order of transforms, expected %v, received %v",
				changes, tform.Insert)
		}
		changes++
	}
	if seen != expecting {
		t.Errorf("Good client didn't receive all expected transforms: %v != %v", expecting, seen)
	}
	wg.Done()
}

func TestClients(t *testing.T) {
	errChan := make(chan BinderError)
	doc, _ := store.NewDocument("hello world")
	logger, stats := loggerAndStats()

	config := DefaultBinderConfig()
	config.FlushPeriod = 5000

	wg := sync.WaitGroup{}

	binder, err := NewBinder(
		doc.ID,
		&testStore{documents: map[string]store.Document{doc.ID: *doc}},
		DefaultBinderConfig(),
		errChan,
		logger,
		stats,
	)
	if err != nil {
		t.Errorf("error: %v", err)
		return
	}

	go func() {
		for err := range errChan {
			t.Errorf("From error channel: %v", err.Err)
		}
	}()

	tform := func(i int) OTransform {
		return OTransform{
			Position: 0,
			Version:  i,
			Delete:   0,
			Insert:   fmt.Sprintf("%v", i),
		}
	}

	portal := binder.Subscribe("")

	if v, err := portal.SendTransform(tform(portal.Version+1), time.Second); v != 2 || err != nil {
		t.Errorf("Send Transform error, v: %v, err: %v", v, err)
	}

	wg.Add(10)
	tformToSend := 50

	for i := 0; i < 10; i++ {
		go goodClient(binder.Subscribe(""), tformToSend, t, &wg)
		//go badClient(binder.Subscribe(""), t, &wg)
	}

	wg.Add(tformToSend)

	for i := 0; i < tformToSend; i++ {
		go goodClient(binder.Subscribe(""), tformToSend-i, t, &wg)
		//go badClient(binder.Subscribe(""), t, &wg)
		if v, err := portal.SendTransform(tform(i+3), time.Second); v != i+3 || err != nil {
			t.Errorf("Send Transform error, expected v: %v, got v: %v, err: %v", i+3, v, err)
		}
	}

	binder.Close()

	wg.Wait()
}

type binderStory struct {
	Content    string       `json:"content" yaml:"content"`
	Transforms []OTransform `json:"transforms" yaml:"transforms"`
	TCorrected []OTransform `json:"corrected_transforms" yaml:"corrected_transforms"`
	Result     string       `json:"result" yaml:"result"`
}

type binderStoriesContainer struct {
	Stories []binderStory `json:"binder_stories" yaml:"binder_stories"`
}

func goodStoryClient(b BinderPortal, bstory *binderStory, wg *sync.WaitGroup, t *testing.T) {
	tformIndex, lenCorrected := 0, len(bstory.TCorrected)
	go func() {
		for tform := range b.TransformRcvChan {
			if tform.Version != bstory.TCorrected[tformIndex].Version ||
				tform.Insert != bstory.TCorrected[tformIndex].Insert ||
				tform.Delete != bstory.TCorrected[tformIndex].Delete ||
				tform.Position != bstory.TCorrected[tformIndex].Position {
				t.Errorf("Transform not expected, %v != %v", tform, bstory.TCorrected[tformIndex])
			}
			tformIndex++
			if tformIndex == lenCorrected {
				wg.Done()
				return
			}
		}
		t.Errorf("channel was closed before receiving last change")
		wg.Done()
		return
	}()
}

func TestBinderStories(t *testing.T) {
	nClients := 10
	logger, stats := loggerAndStats()

	bytes, err := ioutil.ReadFile("../test/stories/binder_stories.js")
	if err != nil {
		t.Errorf("Read file error: %v", err)
		return
	}

	var scont binderStoriesContainer
	if err := json.Unmarshal(bytes, &scont); err != nil {
		t.Errorf("Story parse error: %v", err)
		return
	}

	for _, story := range scont.Stories {
		doc, err := store.NewDocument(story.Content)
		if err != nil {
			t.Errorf("error: %v", err)
			continue
		}

		config := DefaultBinderConfig()
		//config.LogVerbose = true

		errChan := make(chan BinderError)
		go func() {
			for err := range errChan {
				t.Errorf("From error channel: %v", err.Err)
			}
		}()

		binder, err := NewBinder(
			doc.ID,
			&testStore{documents: map[string]store.Document{doc.ID: *doc}},
			config,
			errChan,
			logger,
			stats,
		)
		if err != nil {
			t.Errorf("error: %v", err)
			continue
		}

		wg := sync.WaitGroup{}
		wg.Add(nClients)

		for j := 0; j < nClients; j++ {
			goodStoryClient(binder.Subscribe(""), &story, &wg, t)
		}

		time.Sleep(10 * time.Millisecond)

		bp := binder.Subscribe("")
		go func() {
			for _ = range bp.TransformRcvChan {
			}
		}()

		for j := 0; j < len(story.Transforms); j++ {
			if _, err = bp.SendTransform(story.Transforms[j], time.Second); err != nil {
				t.Errorf("Send issue %v", err)
			}
		}

		wg.Wait()

		newClient := binder.Subscribe("")
		if got, exp := newClient.Document.Content, story.Result; got != exp {
			t.Errorf("Wrong result, expected: %v, received: %v", exp, got)
		}

		binder.Close()
	}
}
