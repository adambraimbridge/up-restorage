package main

import (
	"errors"
	"log"

	"fmt"

	"github.com/kr/pretty"
	"github.com/pborman/uuid"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type mongoEngine struct {
	session    *mgo.Session
	dbName     string
	isBinaryId bool
}

func (eng mongoEngine) Close() {
	// TODO
}

// NewMongoEngine returns an Engine based on a mongodb database backend
func NewMongoEngine(dbName string, collections map[string]Collection, hostPorts string, isBinaryId bool) Engine {
	log.Printf("connecting to mongodb '%s'\n", hostPorts)
	s, err := mgo.Dial(hostPorts)
	if err != nil {
		panic(err)
	}
	s.SetMode(mgo.Monotonic, true)
	eng := &mongoEngine{
		session:    s,
		dbName:     dbName,
		isBinaryId: isBinaryId,
	}

	for _, coll := range collections {
		eng.EnsureIndexes(coll)
	}

	return eng
}

func (eng *mongoEngine) EnsureIndexes(collection Collection) {
	c := eng.session.DB(eng.dbName).C(collection.name)

	eng.session.ResetIndexCache()

	// create collection if it's not there
	c.Create(&mgo.CollectionInfo{})

	err := c.EnsureIndex(mgo.Index{
		Key:        []string{collection.idPropertyName},
		Unique:     true,
		DropDups:   true,
		Background: false,
		Sparse:     false,
	})
	if err != nil {
		log.Printf("failed to create index by %s on %s : %v\n", collection.name, collection.idPropertyName, err.Error())
	}

}

func (eng *mongoEngine) Drop(collection Collection) (bool, error) {
	err := eng.session.DB(eng.dbName).C(collection.name).DropCollection()
	if err != nil {
		log.Printf("failed to drop collection")
	}
	eng.EnsureIndexes(collection)
	//TODO implement error handling and whether drop is successful or not
	return true, nil
}

func (eng *mongoEngine) Write(collection Collection, id string, cont Document) error {
	coll := eng.session.DB(eng.dbName).C(collection.name)
	if id == "" {
		return errors.New("missing id")
	}
	_, err := coll.Upsert(bson.D{{collection.idPropertyName, id}}, cont)
	if err != nil {
		log.Printf("insert failed: %v\n", err)
	}
	return nil
}

func (eng *mongoEngine) Count(collection Collection) (int, error) {
	return eng.session.DB(eng.dbName).C(collection.name).Count()
}

func (eng *mongoEngine) Read(collection Collection, id string) (bool, Document, error) {
	c := eng.session.DB(eng.dbName).C(collection.name)
	var content Document

	var err error
	if eng.isBinaryId {
		binaryId := bson.Binary{Kind: 0x04, Data: []byte(uuid.Parse(id))}
		err = c.Find(bson.M{collection.idPropertyName: binaryId}).One(&content)
	} else {
		err = c.Find(bson.M{collection.idPropertyName: id}).One(&content)
	}

	if err == mgo.ErrNotFound {
		return false, Document{}, nil
	}
	if err != nil {
		return false, Document{}, err
	}
	cleanup(content)
	if eng.isBinaryId {
		content[collection.idPropertyName] = id
	}
	return true, content, nil
}

func (eng *mongoEngine) Delete(collection Collection, id string) error {
	c := eng.session.DB(eng.dbName).C(collection.name)
	err := c.Remove(bson.M{collection.idPropertyName: id})
	if err != mgo.ErrNotFound {
		return err
	}
	return nil
}

func (eng mongoEngine) All(collection Collection, stopchan chan struct{}) (chan Document, error) {
	cont := make(chan Document)

	go func() {
		defer close(cont)
		coll := eng.session.DB(eng.dbName).C(collection.name)
		iter := coll.Find(nil).Iter()
		result := &Document{}
		for iter.Next(result) {
			cleanup(*result)
			select {
			case <-stopchan:
				break
			case cont <- *result:
				result = &Document{}
			}
		}
		if err := iter.Close(); err != nil {
			panic(err)
		}
	}()

	return cont, nil
}

func (eng mongoEngine) Ids(collection Collection, stopchan chan struct{}) (chan string, error) {
	ids := make(chan string)
	go func() {
		defer close(ids)
		coll := eng.session.DB(eng.dbName).C(collection.name)
		iter := coll.Find(nil).Select(bson.M{collection.idPropertyName: true}).Iter()
		var result map[string]interface{}
		for iter.Next(&result) {
			select {
			case <-stopchan:
				break
			case ids <- getUUIDString(result[collection.idPropertyName]):
			}
		}
		if err := iter.Close(); err != nil {
			panic(err)
		}
	}()
	return ids, nil
}

func getUUIDString(uuidValue interface{}) string {
	if uuidString, ok := uuidValue.(string); ok {
		return uuidString
	} else if binaryId, ok := uuidValue.(bson.Binary); ok {
		return uuid.UUID(binaryId.Data).String()
	} else {
		fmt.Printf("UUID field is in an unknown format!\n %# v", pretty.Formatter(uuidValue))
		return ""
	}
}

func cleanup(doc Document) {
	delete(doc, "_id")
}
