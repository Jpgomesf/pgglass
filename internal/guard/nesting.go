package guard

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// maxNesting caps nestingBound. The parser's WebAssembly stack overflows on
// parse trees between 6000 and 7000 messages deep on linux/amd64 (about twice
// that on arm64), and an overflow can leave its pooled instance spinning
// forever on a later parse. Real queries stay far below the cap: a
// 10000-value IN list scores about 30, a 600-term OR chain about 2400.
const maxNesting = 3000

// Weights for nestingBound, in protobuf messages of the parse tree: a
// bracketed group adds at most groupWeight levels (a scalar subquery adds 6),
// an operator or keyword at most opWeight (an A_Expr adds 2), and the
// statement's wrappers plus the leaf at the bottom at most baseWeight.
const (
	groupWeight = 6
	opWeight    = 2
	baseWeight  = 12
)

// checkNesting rejects SQL whose parse tree could be too deep for the parser,
// before the parser sees it.
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

// nestingBound returns an upper bound on the depth of sql's parse tree from
// its tokens, using the Postgres lexer, which does not recurse. Lists are
// flat, so commas and semicolons separate elements whose depths do not add
// up. Within an element, every operator or keyword can add a level, and so
// can the deepest bracketed group in it.
func nestingBound(sql string) (int, error) {
	scan, err := safely(pgquery.Scan, sql)
	if err != nil {
		return 0, err
	}
	stack := []frame{{}}
	for _, tok := range scan.GetTokens() {
		stack = push(stack, tok.GetToken())
	}
	for len(stack) > 1 { // brackets left open; the parser rejects them
		stack = closeGroup(stack)
	}
	return stack[0].depth() + baseWeight, nil
}

// frame tracks one bracketed group, or the whole statement at the bottom of
// the stack.
type frame struct {
	ops   int // weight of the operators and keywords in the current element
	child int // deepest group closed in the current element
	best  int // deepest finished element
}

func (f frame) depth() int {
	return max(f.best, f.ops+f.child)
}

// push adds one token to the stack of open groups.
func push(stack []frame, tok pg_query.Token) []frame {
	top := &stack[len(stack)-1]
	switch tok {
	case pg_query.Token_ASCII_40, pg_query.Token_ASCII_91: // ( [
		return append(stack, frame{})
	case pg_query.Token_ASCII_41, pg_query.Token_ASCII_93: // ) ]
		if len(stack) > 1 {
			return closeGroup(stack)
		}
		top.ops += opWeight // unbalanced; the parser rejects it
	case pg_query.Token_ASCII_44, pg_query.Token_ASCII_59: // , ;
		top.best, top.ops, top.child = top.depth(), 0, 0
	case pg_query.Token_IDENT, pg_query.Token_UIDENT, pg_query.Token_ICONST,
		pg_query.Token_FCONST, pg_query.Token_SCONST, pg_query.Token_USCONST,
		pg_query.Token_BCONST, pg_query.Token_XCONST, pg_query.Token_PARAM,
		pg_query.Token_SQL_COMMENT, pg_query.Token_C_COMMENT:
		// Names, constants and comments are leaves, or not in the tree at all.
	default:
		top.ops += opWeight
	}
	return stack
}

// closeGroup pops the innermost group into the element that contains it.
func closeGroup(stack []frame) []frame {
	inner := stack[len(stack)-1].depth() + groupWeight
	stack = stack[:len(stack)-1]
	top := &stack[len(stack)-1]
	top.child = max(top.child, inner)
	return stack
}
