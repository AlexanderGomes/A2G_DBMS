package queryengine

import (
	"disk-db/storage"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Query struct {
	Result  []storage.Row
	Message string
}

type QueryEngine struct {
	DB *storage.BufferPoolManager
}

const (
	MAX_ROW_SIZE_BYTES = 150
)

func (qe *QueryEngine) QueryEntryPoint(sql string) (Query, error) {
	parsedSQL, err := Parser(sql)
	if err != nil {
		return Query{}, fmt.Errorf("QueryEntryPoint: %w", err)
	}

	queryPlan := GenerateQueryPlan(parsedSQL)

	result, err := qe.ExecuteQueryPlan(queryPlan, parsedSQL)
	if err != nil {
		return Query{}, fmt.Errorf("QueryEntryPoint: %w", err)
	}

	return result, nil
}

func (qe *QueryEngine) ExecuteQueryPlan(qp ExecutionPlan, P *ParsedQuery) (Query, error) {
	var tableDataFile *os.File
	var err error

	query := Query{}
	tablesPtr := []*os.File{}
	tableObj := storage.TableObj{}

	for _, steps := range qp.Steps {
		if err != nil {
			return Query{}, fmt.Errorf("ExecuteQueryPlan: %w", err)
		}

		switch steps.Operation {
		case "GetTable":
			tableObj, tableDataFile, err = GetTable(P, qe.DB, steps)
		case "GetAllColumns":
			err = GetAllColumns(tableDataFile, &query)
		case "CollectPointer":
			tablesPtr = append(tablesPtr, tableDataFile)
		case "FilterByColumns":
			err = FilterByColumns(tableDataFile, &query, P)
		case "InsertRows":
			err = InsertRows(P, &query, qe.DB, tableDataFile)
		case "CreateTable":
			err = CreateTable(P, &query, qe.DB)
		case "JoinQueryTable":
			err = JoinTables(&query, P.Joins[0].Condition, tablesPtr)
		case "DeleteFromTable":
			err = DeleteFromTable(P, qe.DB.DiskScheduler.DiskManager, &tableObj)
		case "WhereClause":
			err = WhereClause(P, &query)
		}
	}

	return query, err
}

func WhereClause(p *ParsedQuery, q *Query) error {
	if len(p.Predicates) < 3 {
		return errors.New("WhereClause (insufficient predicates)")
	}

	field, ok := p.Predicates[0].(string)
	if !ok {
		return errors.New("WhereClause (first predicate is not a string)")
	}
	condition, ok := p.Predicates[1].(string)
	if !ok {
		return errors.New("WhereClause (second predicate is not a string)")
	}
	value, ok := p.Predicates[2].(string)
	if !ok {
		return errors.New("WhereClause (third predicate is not a string)")
	}

	if condition != "=" {
		return errors.New("WhereClause (unsupported condition)")
	}

	res := []storage.Row{}
	for _, row := range q.Result {
		cleanVal, ok := row.Values[field]
		if !ok {
			return fmt.Errorf("field %s not found in row", field)
		}
		cleanVal = strings.Trim(cleanVal, "'")
		if cleanVal == value {
			res = append(res, row)
		}
	}

	q.Result = res
	return nil
}

func DeleteFromTable(p *ParsedQuery, manager *storage.DiskManagerV2, tableObj *storage.TableObj) error {
	tablePages, err := storage.FullTableScan(tableObj.DataFile)
	if err != nil {
		return fmt.Errorf("DeleteFromTable: %w", err)
	}

	predicateStr := p.Predicates[0].(string)
	comparisonParts := strings.Split(predicateStr, "=")
	field := strings.TrimSpace(comparisonParts[0])
	value := strings.TrimSpace(comparisonParts[1])

	for _, page := range tablePages {
		for id, row := range page.Rows {
			foundRow := row.Values[field] == value
			if foundRow {
				delete(page.Rows, id)
			}
		}

		dirPage := tableObj.DirectoryPage
		offset := dirPage.Mapping[page.ID]

		err := manager.WritePageBack(page, offset, tableObj.DataFile)
		if err != nil {
			return fmt.Errorf("DeleteFromTable: %w", err)
		}
	}

	return nil
}

func JoinTables(query *Query, condition string, tablePtr []*os.File) error {
	var err error
	var slicePage1, slicePage2 []*storage.Page

	slicePage1, err = storage.FullTableScan(tablePtr[0])
	if err != nil {
		return fmt.Errorf("JoinTables (error reading table one): %w ", err)
	}

	slicePage2, err = storage.FullTableScan(tablePtr[1])
	if err != nil {
		return fmt.Errorf("JoinTables (error reading table two): %w ", err)
	}

	comparisonParts := strings.Split(condition, "=")
	leftTableCondition := strings.TrimSpace(comparisonParts[0])
	rightTableCondition := strings.TrimSpace(comparisonParts[1])

	hashTable := make(map[string]storage.Row)

	for _, page := range slicePage1 {
		for _, row := range page.Rows {
			joinKey := row.Values[leftTableCondition]
			hashTable[joinKey] = row
		}
	}

	for _, page := range slicePage2 {
		for _, row := range page.Rows {
			joinKey := row.Values[rightTableCondition]
			if matchedRow, exists := hashTable[joinKey]; exists {
				query.Result = append(query.Result, matchedRow)
			}
		}
	}

	return nil
}

func CreateTable(parsedQuery *ParsedQuery, query *Query, bpm *storage.BufferPoolManager) error {
	table := parsedQuery.TableReferences[0]
	manager := bpm.DiskScheduler.DiskManager
	err := manager.CreateTable(storage.TableName(table), storage.TableInfo{})
	if err != nil {
		return fmt.Errorf("QueryEngine (CreateTable): %w", err)
	}

	fmt.Println("TABLE CREATED")
	return nil
}

func GetTable(parsedQuery *ParsedQuery, bpm *storage.BufferPoolManager, step QueryStep) (storage.TableObj, *os.File, error) {
	manager := bpm.DiskScheduler.DiskManager
	tableNAME := parsedQuery.TableReferences[step.index]

	var tableObj *storage.TableObj
	var err error
	tableObj, found := manager.TableObjs[storage.TableName(tableNAME)]
	if !found {
		tableObj, err = manager.InMemoryTableSetUp(storage.TableName(tableNAME))
		if err != nil {
			return storage.TableObj{}, nil, fmt.Errorf("GetTable: %w", err)
		}
	}

	fmt.Println("GOT TABLE")
	return *tableObj, tableObj.DataFile, err
}

func InsertRows(parsedQuery *ParsedQuery, query *Query, bpm *storage.BufferPoolManager, tablePtr *os.File) error {
	fmt.Println("INSERTING")

	rows := parsedQuery.Predicates[0].(storage.Row)
	updatedPage, err := storage.FindAvailablePage(tablePtr, parsedQuery.TableReferences[0], &rows)
	if err != nil {
		return fmt.Errorf("InsertRows: %w", err)
	}

	manager := bpm.DiskScheduler.DiskManager
	tableObj := manager.TableObjs[storage.TableName(parsedQuery.TableReferences[0])]

	offset, found := tableObj.DirectoryPage.Mapping[updatedPage.ID]

	// # just created the page
	if !found {
		offset, err := manager.WritePageEOF(updatedPage, tableObj)
		if err != nil {
			return fmt.Errorf("InsertRows: %w", err)
		}

		tableObj.DirectoryPage.Mapping[updatedPage.ID] = offset
		err = manager.UpdateDirectoryPageDisk(tableObj.DirectoryPage, tableObj)
		if err != nil {
			return fmt.Errorf("InsertRows: %w", err)
		}

		return nil
	}

	err = manager.WritePageBack(updatedPage, offset, tableObj.DataFile)
	if err != nil {
		return fmt.Errorf("InsertRows: %w", err)
	}

	return nil
}


func createColumnMap(columns []string) map[string]string {
	columnMap := make(map[string]string)

	for _, name := range columns {
		columnMap[name] = name
	}

	return columnMap
}

func FilterByColumns(filePtr *os.File, query *Query, P *ParsedQuery) error {
	columnMap := createColumnMap(P.ColumnsSelected)
	pageSlice, err := storage.FullTableScan(filePtr)

	if err != nil {
		return fmt.Errorf("FilterByColumns: %w", err)
	}

	for _, page := range pageSlice {
		for _, tuple := range page.Rows {
			for key := range tuple.Values {
				if _, found := columnMap[key]; !found {
					delete(tuple.Values, key)
				}
			}

			query.Result = append(query.Result, tuple)
		}
	}

	return nil
}

func GetAllColumns(filePtr *os.File, query *Query) error {
	pageSlice, err := storage.FullTableScan(filePtr)

	if err != nil {
		return fmt.Errorf("GetAllColumns: %w", err)
	}

	for _, page := range pageSlice {
		for _, tuple := range page.Rows {
			query.Result = append(query.Result, tuple)
		}
	}

	return err
}
