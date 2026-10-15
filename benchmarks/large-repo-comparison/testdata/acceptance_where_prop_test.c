/*
 * Hidden acceptance tests for Cypher property-to-property WHERE comparisons.
 * Injected only after competing agents finish; never visible to them.
 *
 * These tests build a small in-memory store and exercise both the parser
 * (must not error on a.prop = b.prop) and the evaluator (must filter rows
 * correctly), for early WHERE and post-WITH WHERE.
 */
#include "../src/cypher/cypher.h"
#include "../src/store/store.h"
#include "test_util.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ── fixture store ───────────────────────────────────────────────── */

static cbm_store_t *make_store(void) {
    cbm_store_t *s = cbm_store_create();
    /* two functions in the same file, one in another file */
    int64_t f1 = cbm_store_upsert_node(s, "Function", "Helper", "demo.Helper", "demo.go", 1, 10,
                                       "{}");
    int64_t f2 = cbm_store_upsert_node(s, "Function", "Caller", "demo.Caller", "demo.go", 20, 30,
                                       "{}");
    int64_t f3 = cbm_store_upsert_node(s, "Function", "Other", "other.Other", "other.go", 1, 5,
                                       "{}");
    int64_t f4 = cbm_store_upsert_node(s, "Function", "Cross", "cross.Cross", "cross.go", 1, 5,
                                       "{}");
    (void)f1;
    /* CALLS: Caller->Helper (same file), Cross->Helper (cross-file) */
    cbm_store_insert_edge(s, f2, f1, "CALLS", "{}");
    cbm_store_insert_edge(s, f4, f1, "CALLS", "{}");
    (void)f3;
    return s;
}

static void free_store(cbm_store_t *s) { cbm_store_destroy(s); }

/* ── parse tests ─────────────────────────────────────────────────── */

static void test_parse_prop_to_prop_eq(void) {
    cbm_cypher_query_t *q = NULL;
    char *err = NULL;
    int rc = cbm_cypher_parse(
        "MATCH (a)-[:CALLS]->(b) WHERE a.file_path = b.file_path RETURN a.name, b.name", &q,
        &err);
    ASSERT_EQ(rc, 0);
    ASSERT_TRUE(err == NULL || err[0] == '\0');
    cbm_cypher_query_free(q);
    free(err);
    printf("PASS test_parse_prop_to_prop_eq\n");
}

static void test_parse_prop_to_prop_neq(void) {
    cbm_cypher_query_t *q = NULL;
    char *err = NULL;
    int rc = cbm_cypher_parse("MATCH (a)-[:CALLS]->(b) WHERE a.name <> b.name RETURN a.name",
                              &q, &err);
    ASSERT_EQ(rc, 0);
    cbm_cypher_query_free(q);
    free(err);
    printf("PASS test_parse_prop_to_prop_neq\n");
}

static void test_parse_prop_to_prop_ordering(void) {
    cbm_cypher_query_t *q = NULL;
    char *err = NULL;
    int rc = cbm_cypher_parse("MATCH (a)-[:CALLS]->(b) WHERE a.line_from >= b.line_from RETURN a.name",
                              &q, &err);
    ASSERT_EQ(rc, 0);
    cbm_cypher_query_free(q);
    free(err);
    printf("PASS test_parse_prop_to_prop_ordering\n");
}

static void test_parse_prop_to_prop_after_with(void) {
    cbm_cypher_query_t *q = NULL;
    char *err = NULL;
    int rc = cbm_cypher_parse(
        "MATCH (a)-[:CALLS]->(b) WITH a, b WHERE a.file_path = b.file_path RETURN a.name", &q,
        &err);
    ASSERT_EQ(rc, 0);
    cbm_cypher_query_free(q);
    free(err);
    printf("PASS test_parse_prop_to_prop_after_with\n");
}

static void test_parse_prop_to_literal_still_works(void) {
    cbm_cypher_query_t *q = NULL;
    char *err = NULL;
    int rc = cbm_cypher_parse("MATCH (a)-[:CALLS]->(b) WHERE b.file_path = 'demo.go' RETURN a.name",
                              &q, &err);
    ASSERT_EQ(rc, 0);
    cbm_cypher_query_free(q);
    free(err);
    printf("PASS test_parse_prop_to_literal_still_works\n");
}

/* ── execute tests ───────────────────────────────────────────────── */

static void test_exec_same_file_calls(void) {
    cbm_store_t *s = make_store();
    cbm_cypher_result_t r;
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WHERE a.file_path = b.file_path "
                                "RETURN a.name, b.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    /* Only Caller->Helper is same-file; Cross->Helper is cross-file. */
    ASSERT_EQ(r.row_count, 1);
    if (r.row_count == 1) {
        ASSERT_TRUE(r.rows[0].values[0] && strcmp(r.rows[0].values[0], "Caller") == 0);
        ASSERT_TRUE(r.rows[0].values[1] && strcmp(r.rows[0].values[1], "Helper") == 0);
    }
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_same_file_calls\n");
}

static void test_exec_cross_file_calls(void) {
    cbm_store_t *s = make_store();
    cbm_cypher_result_t r;
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WHERE a.file_path <> b.file_path "
                                "RETURN a.name, b.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    ASSERT_EQ(r.row_count, 1);
    if (r.row_count == 1) {
        ASSERT_TRUE(r.rows[0].values[0] && strcmp(r.rows[0].values[0], "Cross") == 0);
        ASSERT_TRUE(r.rows[0].values[1] && strcmp(r.rows[0].values[1], "Helper") == 0);
    }
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_cross_file_calls\n");
}

static void test_exec_name_neq(void) {
    cbm_store_t *s = make_store();
    cbm_cypher_result_t r;
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WHERE a.name <> b.name RETURN a.name, b.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    /* Both CALLS edges have a.name <> b.name. */
    ASSERT_EQ(r.row_count, 2);
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_name_neq\n");
}

static void test_exec_with_path_same_file(void) {
    cbm_store_t *s = make_store();
    cbm_cypher_result_t r;
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WITH a, b "
                                "WHERE a.file_path = b.file_path RETURN a.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    ASSERT_EQ(r.row_count, 1);
    if (r.row_count == 1) {
        ASSERT_TRUE(r.rows[0].values[0] && strcmp(r.rows[0].values[0], "Caller") == 0);
    }
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_with_path_same_file\n");
}

static void test_exec_prop_to_literal_unchanged(void) {
    cbm_store_t *s = make_store();
    cbm_cypher_result_t r;
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WHERE b.file_path = 'demo.go' "
                                "RETURN a.name, b.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    /* Only Caller->Helper has b.file_path == demo.go. */
    ASSERT_EQ(r.row_count, 1);
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_prop_to_literal_unchanged\n");
}

static void test_exec_ordering_prop_to_prop(void) {
    cbm_store_t *s = make_store();
    /* Add line_from values via update: Helper line_from=1, Caller line_from=20, Cross line_from=1 */
    cbm_cypher_result_t r;
    /* a.line_from >= b.line_from: Caller(20) >= Helper(1) true; Cross(1) >= Helper(1) true */
    int rc = cbm_cypher_execute(s,
                                "MATCH (a)-[:CALLS]->(b) WHERE a.line_from >= b.line_from "
                                "RETURN a.name",
                                "demo", 0, &r);
    ASSERT_EQ(rc, 0);
    ASSERT_EQ(r.row_count, 2);
    cbm_cypher_result_free(&r);
    free_store(s);
    printf("PASS test_exec_ordering_prop_to_prop\n");
}

/* ── main ────────────────────────────────────────────────────────── */

int main(void) {
    test_parse_prop_to_prop_eq();
    test_parse_prop_to_prop_neq();
    test_parse_prop_to_prop_ordering();
    test_parse_prop_to_prop_after_with();
    test_parse_prop_to_literal_still_works();
    test_exec_same_file_calls();
    test_exec_cross_file_calls();
    test_exec_name_neq();
    test_exec_with_path_same_file();
    test_exec_prop_to_literal_unchanged();
    test_exec_ordering_prop_to_prop();
    printf("ALL ACCEPTANCE TESTS PASSED\n");
    return 0;
}
