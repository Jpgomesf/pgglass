package guard

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

const maxNesting = 3000

const (
	groupWeight = 6
	opWeight    = 2
	baseWeight  = 12
)

func checkNesting(sql string) error {
	bound, err := nestingBound(sql)
	if err != nil {
		return violation("SQL does not parse: %s", parseErrorText(err))
	}
	if bound > maxNesting {
		return violation("SQL is nested too deeply to check: use IN (...) instead of long OR chains, and fewer nested subqueries")
	}
	return nil
}

func nestingBound(sql string) (int, error) {
	scan, err := safely(pgquery.Scan, sql)
	if err != nil {
		return 0, err
	}
	stack := []frame{{}}
	for _, tok := range scan.GetTokens() {
		stack = push(stack, tok.GetToken())
	}
	for len(stack) > 1 {
		stack = closeGroup(stack)
	}
	return stack[0].depth() + baseWeight, nil
}

type frame struct {
	ops   int
	child int
	best  int
}

func (f frame) depth() int {
	return max(f.best, f.ops+f.child)
}

func push(stack []frame, tok pg_query.Token) []frame {
	top := &stack[len(stack)-1]
	switch tok {
	case pg_query.Token_ASCII_40, pg_query.Token_ASCII_91:
		return append(stack, frame{})
	case pg_query.Token_ASCII_41, pg_query.Token_ASCII_93:
		if len(stack) > 1 {
			return closeGroup(stack)
		}
		top.ops += opWeight
	case pg_query.Token_ASCII_44, pg_query.Token_ASCII_59:
		top.best, top.ops, top.child = top.depth(), 0, 0
	case pg_query.Token_IDENT, pg_query.Token_UIDENT, pg_query.Token_ICONST,
		pg_query.Token_FCONST, pg_query.Token_SCONST, pg_query.Token_USCONST,
		pg_query.Token_BCONST, pg_query.Token_XCONST, pg_query.Token_PARAM,
		pg_query.Token_SQL_COMMENT, pg_query.Token_C_COMMENT:
	default:
		top.ops += opWeight
	}
	return stack
}

func closeGroup(stack []frame) []frame {
	inner := stack[len(stack)-1].depth() + groupWeight
	stack = stack[:len(stack)-1]
	top := &stack[len(stack)-1]
	top.child = max(top.child, inner)
	return stack
}
