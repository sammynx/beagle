// Copyright 2019 The DutchSec Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package db

import (
	"database/sql"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// Tx TODO: NEEDS COMMENT INFO
type Tx struct {
	*sqlx.Tx

	stacktrace string
	time       time.Time

	statementsCache sync.Map

	queries []string
}

func (tx *Tx) Preparex(query string) (*sqlx.Stmt, error) {
	tx.queries = append(tx.queries, query)

	if stmt, ok := tx.statementsCache.Load(query); ok {
		return stmt.(*sqlx.Stmt), nil
	}

	stmt, err := tx.Tx.Preparex(query)
	if err != nil {
		return nil, err
	}

	tx.statementsCache.Store(query, stmt)
	return stmt, nil
}

func (tx *Tx) Commit() error {
	err := tx.Tx.Commit()

	if time.Now().Sub(tx.time) > 1*time.Second {
		log.Warning("Transaction commit took long, took: %q, query=%v.", time.Now().Sub(tx.time), tx.queries)
	}

	log.Info("Transaction commit, took: %q.", time.Now().Sub(tx.time))
	return err
}

func (tx *Tx) Rollback() error {
	err := tx.Tx.Rollback()
	log.Info("Transaction rollback, took: %q", time.Now().Sub(tx.time))
	return err
}

// Selectx TODO: NEEDS COMMENT INFO
func (tx *Tx) Selectx(o interface{}, qx Queryx, options ...selectOption) error {
	q := string(qx.Query)
	params := qx.Params

	for _, option := range options {
		q, params = option.Wrap(q, params)
	}

	log.Debug(q)

	if u, ok := o.(Selecter); ok {
		return u.Select(tx.Tx, Query(q), params...)
	}

	// prepared statements should be cached

	stmt, err := tx.Preparex(q)
	if err != nil {
		return err
	}

	return stmt.Select(o, params...)
}

// Countx TODO: NEEDS COMMENT INFO
func (tx *Tx) Countx(qx Queryx) (int, error) {
	stmt, err := tx.Preparex(fmt.Sprintf("SELECT COUNT(*) FROM (%s) q", string(qx.Query)))
	if err != nil {
		return 0, err
	}

	count := 0
	err = stmt.Get(&count, qx.Params...)
	return count, err
}

// Exec TODO: NEEDS COMMENT INFO
func (tx *Tx) Exec(qx Queryx) error {
	stmt, err := tx.Preparex(string(qx.Query))
	if err != nil {
		return err
	}

	_, err = stmt.Exec(qx.Params...)
	return err
}

// Getx TODO: NEEDS COMMENT INFO
func (tx *Tx) Getx(o interface{}, qx Queryx) error {
	if u, ok := o.(Getter); ok {
		return u.Get(tx.Tx, qx)
	}

	log.Error("No getter found for object: %s", reflect.TypeOf(o))
	return ErrNoGetterFound
}

// Get TODO: NEEDS COMMENT INFO
func (tx *Tx) Get(o interface{}, qx Queryx) error {
	if u, ok := o.(Getter); ok {
		return u.Get(tx.Tx, qx)
	}

	log.Error("No getter found for object: %s", reflect.TypeOf(o))
	return ErrNoGetterFound
}

// Update TODO: NEEDS COMMENT INFO
func (tx *Tx) InsertOrUpdate(o interface{}) error {
	if u, ok := o.(InsertOrUpdater); ok {
		return u.InsertOrUpdate(tx.Tx)
	}

	log.Error("No InsertOrUpdate found for object: %s", reflect.TypeOf(o))
	return ErrNoInsertOrUpdaterFound
}

// Update TODO: NEEDS COMMENT INFO
func (tx *Tx) Update(o interface{}) error {
	if u, ok := o.(Updater); ok {
		return u.Update(tx.Tx)
	}

	log.Error("No updater found for object: %s", reflect.TypeOf(o))
	return ErrNoUpdaterFound
}

// Delete TODO: NEEDS COMMENT INFO
func (tx *Tx) Delete(o interface{}) error {
	if u, ok := o.(Deleter); ok {
		return u.Delete(tx.Tx)
	}

	log.Error("No deleter found for object: %s", reflect.TypeOf(o))
	return ErrNoDeleterFound
}

// Insert TODO: NEEDS COMMENT INFO
func (tx *Tx) Insert(o interface{}) error {
	if u, ok := o.(Inserter); ok {
		err := u.Insert(tx.Tx)
		if err != nil {
			log.Error(err.Error())
		}
		return err
	}

	log.Error("No inserter found for object: %s", reflect.TypeOf(o))
	return ErrNoInserterFound
}

type TxOptionFunc func(opt *sql.TxOptions)

func ReadOnly() TxOptionFunc {
	return func(opt *sql.TxOptions) {
		opt.ReadOnly = true
	}
}
