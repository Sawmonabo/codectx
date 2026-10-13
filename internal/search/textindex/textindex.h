/*
 * The C boundary of the embedded search index (docs/adr/ADR-0013-search-index.md).
 *
 * Every function is a coarse batch call: one unit's documents, one commit, one
 * page. Every export catches a panic and reports it as CX_INTERNAL; nothing
 * unwinds across this boundary. A function that fails writes a message into
 * *err (allocated by the library, released with cx_buf_free) and returns a
 * nonzero status; on success *err is left empty.
 *
 * Buffers are little-endian. Integers are LEB128 unsigned varints ("uvarint")
 * unless named otherwise; signed values are zigzag varints ("varint"); a
 * string is a uvarint byte length followed by that many UTF-8 bytes.
 */
#ifndef CODECTX_TEXTINDEX_H
#define CODECTX_TEXTINDEX_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Status codes. */
#define CX_OK 0
#define CX_INVALID 1         /* a malformed argument or buffer */
#define CX_BUSY 2            /* another process holds the index writer */
#define CX_CANCELED 3        /* the cancel flag was raised before any page existed */
#define CX_CORRUPT 4         /* the index directory or a block summary is damaged */
#define CX_DISK_FULL 5       /* the filesystem refused a write for lack of space */
#define CX_INTERNAL 6        /* any other failure, a caught panic included */
#define CX_TOO_MANY_TERMS 7  /* *err holds the query's term count in decimal */
#define CX_NO_POSITION 8     /* a request's resume position names no document of the view */

/* A byte buffer owned by the library. */
typedef struct {
    uint8_t *ptr;
    size_t len;
    size_t cap;
} cx_buf;

typedef struct cx_index cx_index;
typedef struct cx_view cx_view;

typedef struct {
    /* Nonzero: this process may write. The writer is opened on the first
     * mutating call and closed by cx_index_release_writer or cx_index_close. */
    uint8_t writable;
    uint32_t writer_threads;
    /* The writer's whole memory budget, shared across its threads. */
    uint64_t writer_memory_bytes;
    uint32_t merge_threads;
    uint32_t search_threads;
    /* The byte bound of the per-(generation, segment) alive-set cache. */
    uint64_t view_cache_bytes;
} cx_open_options;

/* The smallest writer budget the library accepts for this many threads. */
uint64_t cx_writer_memory_floor(uint32_t threads);

/* Opens (creating when writable and absent) the index in dir. */
int32_t cx_index_open(const uint8_t *dir, size_t dir_len, const cx_open_options *opts,
                      cx_index **out, cx_buf *err);
/* Waits for running merges, builds their summaries, closes the writer and
 * frees the handle. Safe on NULL. */
int32_t cx_index_close(cx_index *x, cx_buf *err);

/*
 * Adds one unit's documents. Layout:
 *   string unit_key
 *   uvarint count
 *   count x { uvarint doc_id, string search_key, string node_id,
 *             string file_id, string path, string kind, string name,
 *             string qualified_name, string signature,
 *             uvarint start_byte, uvarint end_byte, string body }
 */
int32_t cx_index_add(cx_index *x, const uint8_t *buf, size_t len, cx_buf *err);
/* Deletes documents by their doc_id. */
int32_t cx_index_delete_docs(cx_index *x, const uint64_t *ids, size_t n, cx_buf *err);
/* Deletes every document whose doc_id is >= first. */
int32_t cx_index_delete_from(cx_index *x, uint64_t first, cx_buf *err);
/* Deletes every document whose unit_key is unit. */
int32_t cx_index_delete_unit(cx_index *x, const uint8_t *unit, size_t len, cx_buf *err);
/* Commits every pending change with an opaque payload, then builds the block
 * summary of every searchable segment that has none. */
int32_t cx_index_commit(cx_index *x, const uint8_t *payload, size_t len, cx_buf *err);
/* Waits for running merges, builds the summaries of the segments they
 * produced, removes summaries of segments that no longer exist, and closes
 * the writer. A later mutating call opens it again. */
int32_t cx_index_release_writer(cx_index *x, cx_buf *err);
/* The payload of the last commit, empty when nothing was ever committed. */
int32_t cx_index_committed(cx_index *x, cx_buf *payload, cx_buf *err);
/* The number of committed, not deleted documents whose unit_key is unit. */
int32_t cx_index_unit_docs(cx_index *x, const uint8_t *unit, size_t len, uint64_t *count, cx_buf *err);
/* Merges every searchable segment into one and builds its summary. */
int32_t cx_index_merge_all(cx_index *x, cx_buf *err);

/*
 * Opens a view of one generation over the latest commit. visible is a bitmap
 * over doc ids: bit (id % 8) of byte (id / 8), least significant first, set
 * when the document is visible. The view reloads the reader first, so a
 * generation activated by another process after this one opened is seen.
 */
int32_t cx_view_open(cx_index *x, uint64_t generation, const uint8_t *visible, size_t len,
                     cx_view **out, cx_buf *err);
void cx_view_close(cx_view *v);

/*
 * Answers one page: the best `depth` folded results, by score descending and
 * then the content key (path, start byte, end byte, kind, name, qualified name,
 * signature, node id, search key), after the resume position when one is given.
 * Request layout:
 *   string query
 *   uvarint kinds,    kinds x string
 *   uvarint prefixes, prefixes x string   (cleaned root-relative path prefixes)
 *   uvarint depth                         (folded results wanted, >= 1)
 *   u8      length_rule                   (0: total, 1: weighted per column)
 *   uvarint max_terms                     (0: unlimited)
 *   u8      flags                         (bit 0: ignore every block summary;
 *                                          bit 1: a resume position follows)
 *   [varint score_micros, uvarint doc_id] (with bit 1: the score and doc_id of
 *                                          a result a page of this view served;
 *                                          the page holds only results whose
 *                                          best document sorts strictly after
 *                                          that document)
 * Page layout:
 *   uvarint terms
 *   u8      more          (1: more results exist past depth)
 *   u8      canceled      (1: the cancel flag ended the search; the page holds
 *                          the best results found before it)
 *   uvarint matches       (matching visible documents counted)
 *   u8      matches_exact (1: every match was visited)
 *   uvarint segments, uvarint segments_without_summary,
 *   uvarint blocks_total, uvarint blocks_scored, uvarint docs_scored
 *   uvarint heaps         (segments scored at once)
 *   uvarint cap           (heap places per segment, after any rescoring)
 *   uvarint marked        (groups the resume position marked as served)
 *   uvarint hits, hits x { uvarint doc_id, string search_key, string node_id, string file_id,
 *                          string path, string kind, string name,
 *                          string qualified_name, string signature,
 *                          uvarint start_byte, uvarint end_byte,
 *                          varint score_micros, uvarint occurrences,
 *                          u8 occurrences_exact }
 * cancel points at one byte the caller may set to nonzero from another thread
 * while the search runs; the search reads it between blocks.
 */
int32_t cx_view_search(cx_view *v, const uint8_t *req, size_t len, const volatile uint8_t *cancel,
                       cx_buf *page, cx_buf *err);

/*
 * The scorer's arithmetic on raw inputs, for the cross-language agreement
 * test. Input: count x { u64 docs, u64 df, f64 wtf, u64 dl, f64 avgdl }, 40
 * bytes each. Output: count x { f64 idf, f64 score, i64 micros }: idf is the
 * pinned logarithm of 1 + (docs - df + 0.5) / (df + 0.5), score is idf times
 * the saturation of wtf at length dl, and micros is score quantized to 1e-6.
 */
int32_t cx_score_vector(const uint8_t *in, size_t len, cx_buf *out, cx_buf *err);
/* Panics inside the export guard and returns the caught panic as CX_INTERNAL. */
int32_t cx_panic_selftest(cx_buf *err);

void cx_buf_free(cx_buf *b);

#ifdef __cplusplus
}
#endif

#endif
