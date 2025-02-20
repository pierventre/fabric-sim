// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

// Package entries contains implementation of various P4 entitites such as tables, groups, meters, etc.
package entries

import (
	"crypto/sha1"
	"github.com/onosproject/onos-lib-go/pkg/errors"
	p4info "github.com/p4lang/p4runtime/go/p4/config/v1"
	p4api "github.com/p4lang/p4runtime/go/p4/v1"
	"hash"
	"sort"
)

//var log = logging.GetLogger("simulator", "entries")

// BatchSender is an abstract function for returning batches of read entities
type BatchSender func(entities []*p4api.Entity) error

// Table represents a single P4 table
type Table struct {
	info       *p4info.Table
	rows       map[string]*Row
	defaultRow *Row
}

// Tables represents a set of P4 tables
type Tables struct {
	tables map[uint32]*Table
}

// Row represents table row entry and its mutable direct resources
type Row struct {
	entry       *p4api.TableEntry
	counterData *p4api.CounterData
	meterConfig *p4api.MeterConfig
	meterData   *p4api.MeterCounterData
}

// ReadType specifies whether to read table entry, its direct counter or its direct meter
type ReadType byte

const (
	// ReadTableEntry specified that reads should return entities with TableEntry
	ReadTableEntry ReadType = iota
	// ReadDirectCounter specified that reads should return entities with DirectCounterEntry
	ReadDirectCounter
	// ReadDirectMeter specified that reads should return entities with DirectMeterEntry
	ReadDirectMeter
)

// NewTables creates a new set of tables from the given P4 info descriptor
func NewTables(tablesInfo []*p4info.Table) *Tables {
	ts := &Tables{
		tables: make(map[uint32]*Table),
	}
	for _, ti := range tablesInfo {
		ts.tables[ti.Preamble.Id] = ts.NewTable(ti)
	}
	return ts
}

// NewTable creates a new device table
func (ts *Tables) NewTable(table *p4info.Table) *Table {
	// Sort the fields into canonical order based on ID
	sort.SliceStable(table.MatchFields, func(i, j int) bool { return table.MatchFields[i].Id < table.MatchFields[j].Id })
	return &Table{
		info: table,
		rows: make(map[string]*Row),
	}
}

// Creates a new table row from the specified table entry
func (t *Table) newRow(entry *p4api.TableEntry) *Row {
	row := &Row{entry: entry, meterConfig: entry.MeterConfig, counterData: &p4api.CounterData{}}
	if entry.CounterData != nil {
		row.counterData = entry.CounterData
	}
	if entry.MeterCounterData != nil {
		row.meterData = entry.MeterCounterData
	}
	return row
}

// Tables returns the list of tables
func (ts *Tables) Tables() []*Table {
	tables := make([]*Table, 0, len(ts.tables))
	for _, table := range ts.tables {
		tables = append(tables, table)
	}
	return tables
}

// ModifyTableEntry modifies the specified table entry in its appropriate table
func (ts *Tables) ModifyTableEntry(entry *p4api.TableEntry, insert bool) error {
	table, ok := ts.tables[entry.TableId]
	if !ok {
		return errors.NewNotFound("table %d not found", entry.TableId)
	}
	return table.ModifyTableEntry(entry, insert)
}

// RemoveTableEntry removes the specified table entry from its appropriate table
func (ts *Tables) RemoveTableEntry(entry *p4api.TableEntry) error {
	table, ok := ts.tables[entry.TableId]
	if !ok {
		return errors.NewNotFound("table %d not found", entry.TableId)
	}
	return table.RemoveTableEntry(entry)
}

// ModifyDirectCounterEntry modifies the specified direct counter entry in its appropriate table
func (ts *Tables) ModifyDirectCounterEntry(entry *p4api.DirectCounterEntry, insert bool) error {
	if insert {
		return errors.NewInvalid("direct counter entry cannot be inserted")
	}
	table, ok := ts.tables[entry.TableEntry.TableId]
	if !ok {
		return errors.NewNotFound("table %d not found", entry.TableEntry.TableId)
	}
	return table.ModifyDirectCounterEntry(entry)
}

// ModifyDirectMeterEntry modifies the specified direct meter entry in its appropriate table
func (ts *Tables) ModifyDirectMeterEntry(entry *p4api.DirectMeterEntry, insert bool) error {
	if insert {
		return errors.NewInvalid("direct counter entry cannot be inserted")
	}
	table, ok := ts.tables[entry.TableEntry.TableId]
	if !ok {
		return errors.NewNotFound("table %d not found", entry.TableEntry.TableId)
	}
	return table.ModifyDirectMeterEntry(entry)
}

// ReadTableEntries reads the table entries matching the specified table entry, from the appropriate table
func (ts *Tables) ReadTableEntries(request *p4api.TableEntry, readType ReadType, sender BatchSender) error {
	// If the table ID is 0, read all tables
	if request.TableId == 0 {
		for _, table := range ts.tables {
			if err := table.ReadTableEntries(request, readType, sender); err != nil {
				return err
			}
		}
		return nil
	}

	// Otherwise, locate the desired table and read from it
	table, ok := ts.tables[request.TableId]
	if !ok {
		return errors.NewNotFound("table %d not found", request.TableId)
	}
	return table.ReadTableEntries(request, readType, sender)
}

// Table returns the table with the specified ID
func (ts *Tables) Table(id uint32) *Table {
	return ts.tables[id]
}

// ID returns the table ID
func (t *Table) ID() uint32 {
	return t.info.Preamble.Id
}

// Size returns the number of entries in the table
func (t *Table) Size() int {
	if t.defaultRow != nil {
		return len(t.rows) + 1
	}
	return len(t.rows)
}

// Name returns the table name
func (t *Table) Name() string {
	return t.info.Preamble.Name
}

// Entries returns a copy of the table entries; in no particular order
func (t *Table) Entries() []*p4api.TableEntry {
	entries := make([]*p4api.TableEntry, 0, len(t.rows))
	for _, row := range t.rows {
		entries = append(entries, row.entry)
	}
	if t.defaultRow != nil {
		entries = append(entries, t.defaultRow.entry)
	}
	return entries
}

// ModifyTableEntry inserts or modifies the specified entry
func (t *Table) ModifyTableEntry(entry *p4api.TableEntry, insert bool) error {
	if entry.IsDefaultAction {
		if insert {
			return errors.NewInvalid("unable to insert default action entry")
		}
		if len(entry.Match) > 0 {
			return errors.NewInvalid("default action entry cannot have any match fields")
		}
		t.defaultRow = t.newRow(entry)
		return nil
	}

	// Order field matches in canonical order based on field ID
	sortFieldMatches(entry.Match)

	// Produce a hash of the priority and the field matches to serve as a key
	key, err := t.entryKey(entry)
	if err != nil {
		return err
	}
	row, ok := t.rows[key]

	// If the entry exists, and we're supposed to do a new insert, raise error
	if ok && insert {
		return errors.NewAlreadyExists("entry already exists: %v", entry)
	}

	// If the entry doesn't exist, and we're supposed to modify, raise error
	if !ok && !insert {
		return errors.NewNotFound("entry doesn't exist: %v", entry)
	}

	// If the entry doesn't exist and we're supposed to do insert, well... do it
	if !ok && insert {
		row = t.newRow(entry)
		t.rows[key] = row
	}

	// Otherwise, update the entry and its direct resources
	row.entry = entry
	row.meterConfig = entry.MeterConfig

	// If this is an update and counter data has been given, update it
	if !insert && entry.CounterData != nil {
		row.counterData = entry.CounterData
	}
	return nil
}

// RemoveTableEntry removes the specified table entry and any direct counter data and meter configs for that entry
func (t *Table) RemoveTableEntry(entry *p4api.TableEntry) error {
	if entry.IsDefaultAction {
		return errors.NewInvalid("unable to remove default action entry")
	}
	// Order field matches in canonical order based on field ID
	sortFieldMatches(entry.Match)

	// Produce a hash of the priority and the field matches to serve as a key
	key, err := t.entryKey(entry)
	if err != nil {
		return err
	}
	delete(t.rows, key)
	return nil
}

// ModifyDirectCounterEntry modifies the specified direct counter entry data
func (t *Table) ModifyDirectCounterEntry(entry *p4api.DirectCounterEntry) error {
	// Order field matches in canonical order based on field ID
	sortFieldMatches(entry.TableEntry.Match)

	// Produce a hash of the priority and the field matches to serve as a key
	key, err := t.entryKey(entry.TableEntry)
	if err != nil {
		return err
	}
	row, ok := t.rows[key]
	if !ok {
		return errors.NewNotFound("entry doesn't exist: %v", entry)
	}
	row.counterData = entry.Data
	return nil
}

// ModifyDirectMeterEntry modifies the specified direct meter entry data
func (t *Table) ModifyDirectMeterEntry(entry *p4api.DirectMeterEntry) error {
	// Order field matches in canonical order based on field ID
	sortFieldMatches(entry.TableEntry.Match)

	// Produce a hash of the priority and the field matches to serve as a key
	key, err := t.entryKey(entry.TableEntry)
	if err != nil {
		return err
	}
	row, ok := t.rows[key]
	if !ok {
		return errors.NewNotFound("entry doesn't exist: %v", entry)
	}
	row.meterConfig = entry.Config
	return nil
}

type entityBuffer struct {
	entities []*p4api.Entity
	sender   BatchSender
}

func newBuffer(sender BatchSender) *entityBuffer {
	return &entityBuffer{
		entities: make([]*p4api.Entity, 0, 64),
		sender:   sender,
	}
}

// Sends the specified entity via an accumulation buffer, flushing when buffer reaches capacity
func (eb *entityBuffer) sendEntity(entity *p4api.Entity) error {
	var err error
	eb.entities = append(eb.entities, entity)

	// If we've reached the buffer capacity, flush it
	if len(eb.entities) == cap(eb.entities) {
		err = eb.flush()
	}
	return err
}

// Flushes the buffer by sending the buffered entities and resets the buffer
func (eb *entityBuffer) flush() error {
	err := eb.sender(eb.entities)
	eb.entities = eb.entities[:0]
	return err
}

// ReadTableEntries reads the table entries matching the specified table entry request
func (t *Table) ReadTableEntries(request *p4api.TableEntry, readType ReadType, sender BatchSender) error {
	// TODO: implement exact match
	buffer := newBuffer(sender)

	// Otherwise, iterate over all entries, matching each against the request
	for _, row := range t.rows {
		if t.tableEntryMatches(request, row.entry) {
			if err := buffer.sendEntity(getEntry(readType, row)); err != nil {
				return err
			}
		}
	}
	if t.defaultRow != nil {
		if err := buffer.sendEntity(getEntry(readType, t.defaultRow)); err != nil {
			return err
		}
	}
	return buffer.flush()
}

// Get the entity with the entry typed according to the specified read type
func getEntry(readType ReadType, row *Row) *p4api.Entity {
	switch readType {
	case ReadDirectCounter:
		return &p4api.Entity{Entity: &p4api.Entity_DirectCounterEntry{DirectCounterEntry: &p4api.DirectCounterEntry{
			TableEntry: row.entry,
			Data:       row.counterData,
		}}}
	case ReadDirectMeter:
		return &p4api.Entity{Entity: &p4api.Entity_DirectMeterEntry{DirectMeterEntry: &p4api.DirectMeterEntry{
			TableEntry:  row.entry,
			Config:      row.meterConfig,
			CounterData: row.meterData,
		}}}
	}
	return &p4api.Entity{Entity: &p4api.Entity_TableEntry{TableEntry: row.entry}}
}

func (t *Table) tableEntryMatches(request *p4api.TableEntry, entry *p4api.TableEntry) bool {
	// TODO: implement full spectrum of wildcard matching
	return true
}

// Produces a table entry key using a uint64 hash of its field matches; returns error if the matches do not comply
// with the table schema
func (t *Table) entryKey(entry *p4api.TableEntry) (string, error) {
	hf := sha1.New()

	// This assumes matches have already been put in canonical order
	for i, m := range entry.Match {
		// Validate field ID against the P4Info table schema
		if err := t.validateMatch(i, m); err != nil {
			return "", err
		}
		switch {
		case m.GetExact() != nil:
			_, _ = hf.Write([]byte{0x01})
			_, _ = hf.Write(m.GetExact().Value)
		case m.GetLpm() != nil:
			_, _ = hf.Write([]byte{0x02})
			writeHash(hf, m.GetLpm().PrefixLen)
			_, _ = hf.Write(m.GetLpm().Value)
		case m.GetRange() != nil:
			_, _ = hf.Write([]byte{0x03})
			_, _ = hf.Write(m.GetRange().Low)
			_, _ = hf.Write(m.GetRange().High)
		case m.GetTernary() != nil:
			_, _ = hf.Write([]byte{0x04})
			_, _ = hf.Write(m.GetTernary().Mask)
			_, _ = hf.Write(m.GetTernary().Value)
		case m.GetOptional() != nil:
			_, _ = hf.Write([]byte{0x05})
			_, _ = hf.Write(m.GetOptional().Value)
		}
	}
	return string(hf.Sum(nil)), nil
}

// Validates that the specified match corresponds to the expected table schema
func (t *Table) validateMatch(i int, m *p4api.FieldMatch) error {
	if i >= len(t.info.MatchFields) {
		return errors.NewInvalid("unexpected field match %d: %v", i, m)
	}

	// TODO: implement validation that the match is of expected type
	return nil
}

func writeHash(hash hash.Hash, n int32) {
	_, _ = hash.Write([]byte{byte((n & 0xff0000) >> 24), byte((n & 0xff0000) >> 16), byte((n & 0xff00) >> 8), byte(n & 0xff)})
}

// Sorts the given array of field matches in place based on the field ID
func sortFieldMatches(matches []*p4api.FieldMatch) {
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].FieldId < matches[j].FieldId })
}
