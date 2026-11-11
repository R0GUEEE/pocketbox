// archive_shim.c — the slice of libarchive the engine needs, without libarchive.
//
// Building libarchive for arm64 iOS needs a working CMake toolchain and several
// Darwin-source patches. Everything git, curl and tar need is a fraction of the
// real library, so this file implements exactly the API surface fakefsify uses
// (see tools/fakefs.c) for tar and tar.gz, which is the only format the guest
// rootfs ships in. Any other format fails loudly at open time.

#include <archive.h>
#include <archive_entry.h>

#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#ifdef __APPLE__
#include <zlib.h>
#else
#include <zlib.h>
#endif

struct shim_entry {
    char *path;
    char *linkname;      /* symlink target or hardlink source */
    char *symlink;
    char *hardlink;
    int64_t size;
    int64_t consumed;
    mode_t mode;
    int64_t uid, gid;
    int64_t rdev;
    time_t atime, mtime, ctime;
    long atime_nsec, mtime_nsec, ctime_nsec;
    int atime_set, mtime_set;
    unsigned type;
};

struct archive {
    int is_write;
    int closed;
    int err;
    char errmsg[256];

    /* read side */
    int fd;
    uint8_t *data;
    size_t len, pos;
    int gz;
    z_stream zs;
    uint8_t outbuf[65536];
    int eof;

    /* write side */
    char *outpath;
    FILE *out;
    gzFile gzw;

    /* current entry */
    struct shim_entry entry;
    int have_entry;
    char *pending_link;
};

static void set_err(struct archive *a, int err, const char *msg) {
    a->err = err;
    snprintf(a->errmsg, sizeof(a->errmsg), "%s", msg ? msg : "error");
}

/* ---- tar parsing ------------------------------------------------------- */

struct tar_hdr {
    char name[100];
    char mode[8];
    char uid[8];
    char gid[8];
    char size[12];
    char mtime[12];
    char chksum[8];
    char typeflag;
    char linkname[100];
    char magic[6];
    char version[2];
    char uname[32];
    char gname[32];
    char devmajor[8];
    char devminor[8];
    char prefix[155];
    char pad[12];
};
_Static_assert(sizeof(struct tar_hdr) == 512, "tar header is 512 bytes");

static int64_t octal(const char *p, size_t n) {
    int64_t v = 0;
    for (size_t i = 0; i < n; i++) {
        if (p[i] == ' ' || p[i] == '\0')
            continue;
        if (p[i] < '0' || p[i] > '7')
            return v;
        v = v * 8 + (p[i] - '0');
    }
    return v;
}

static int read_exact(struct archive *a, void *buf, size_t n) {
    memcpy(buf, a->data + a->pos, n);
    a->pos += n;
    return 0;
}

/* Returns 1 for an entry, 0 for end of archive, -1 on error. */
static int next_entry(struct archive *a, struct archive_entry **out) {
    if (a->pos + 512 > a->len) {
        a->eof = 1;
        return 0;
    }
    struct tar_hdr h;
    read_exact(a, &h, 512);
    int all_zero = 1;
    for (size_t i = 0; i < 512; i++) {
        if (((uint8_t *) &h)[i]) { all_zero = 0; break; }
    }
    if (all_zero) {
        a->eof = 1;
        return 0;
    }
    if (memcmp(h.magic, "ustar", 5) != 0) {
        set_err(a, EINVAL, "unsupported tar variant");
        return -1;
    }
    struct shim_entry *e = &a->entry;
    free(e->path); free(e->hardlink); free(e->symlink);
    memset(e, 0, sizeof(*e));
    char path[256];
    path[0] = '\0';
    if (h.prefix[0] != '\0') {
        snprintf(path, sizeof(path), "%.*s/%.*s", 155, h.prefix, 100, h.name);
    } else {
        snprintf(path, sizeof(path), "%.*s", 100, h.name);
    }
    e->path = strdup(path);
    e->size = octal(h.size, 12);
    e->mode = (mode_t) octal(h.mode, 8);
    e->uid = octal(h.uid, 8);
    e->gid = octal(h.gid, 8);
    e->mtime = (time_t) octal(h.mtime, 12);
    e->mtime_set = 1;
    e->rdev = octal(h.devmajor, 8) << 8 | octal(h.devminor, 8);
    switch (h.typeflag) {
        case '0': case '\0': e->type = S_IFREG; break;
        case '1': e->type = S_IFREG; e->hardlink = strndup(h.linkname, 100); break;
        case '2': e->type = S_IFLNK; e->symlink = strndup(h.linkname, 100); break;
        case '3': e->type = S_IFCHR; break;
        case '4': e->type = S_IFBLK; break;
        case '5': e->type = S_IFDIR; break;
        case '6': e->type = S_IFIFO; break;
        case 'x': case 'g': case 'L': case 'K': case 'V':
            /* pax/gnu extension records: skip payload and continue */
            a->pos += (size_t) ((e->size + 511) & ~(off_t) 511);
            return next_entry(a, out);
        default: e->type = S_IFREG; break;
    }
    a->have_entry = 1;
    *out = (struct archive_entry *) e;
    return 1;
}

/* ---- read API ---------------------------------------------------------- */

struct archive *archive_read_new(void) {
    struct archive *a = calloc(1, sizeof(*a));
    return a;
}

int archive_read_support_filter_gzip(struct archive *a) { (void) a; return ARCHIVE_OK; }
int archive_read_support_filter_bzip2(struct archive *a) { return ARCHIVE_WARN; }
int archive_read_support_format_tar(struct archive *a) { (void) a; return ARCHIVE_OK; }

int archive_read_open_filename(struct archive *a, const char *path, size_t block) {
    (void) block;
    a->fd = open(path, O_RDONLY);
    if (a->fd < 0) {
        set_err(a, errno, strerror(errno));
        return ARCHIVE_FATAL;
    }
    struct stat st;
    if (fstat(a->fd, &st) < 0 || st.st_size <= 0) {
        set_err(a, EINVAL, "cannot stat archive");
        return ARCHIVE_FATAL;
    }
    a->data = malloc((size_t) st.st_size);
    ssize_t off = 0;
    while (off < st.st_size) {
        ssize_t n = read(a->fd, a->data + off, (size_t) (st.st_size - off));
        if (n <= 0)
            break;
        off += n;
    }
    close(a->fd);
    a->fd = -1;
    a->len = (size_t) off;
    a->pos = 0;
    if (a->len > 2 && a->data[0] == 0x1f && a->data[1] == 0x8b) {
        if (inflateInit2(&a->zs, 15 + 32) != Z_OK) {
            set_err(a, EINVAL, "zlib init failed");
            return ARCHIVE_FATAL;
        }
        a->gz = 1;
        /* Decompress fully: rootfs tarballs are a few MB at most. */
        size_t cap = a->len * 4 + 1024;
        uint8_t *out = malloc(cap);
        size_t used = 0;
        a->zs.next_in = a->data;
        a->zs.avail_in = (uInt) a->len;
        int rc;
        do {
            if (used == cap) {
                cap *= 2;
                out = realloc(out, cap);
            }
            a->zs.next_out = out + used;
            a->zs.avail_out = (uInt) (cap - used);
            rc = inflate(&a->zs, Z_NO_FLUSH);
            used = cap - a->zs.avail_out;
            if (rc == Z_STREAM_END)
                break;
            if (rc != Z_OK && rc != Z_BUF_ERROR) {
                free(out);
                set_err(a, EINVAL, "gzip stream corrupt");
                return ARCHIVE_FATAL;
            }
        } while (a->zs.avail_in > 0);
        inflateEnd(&a->zs);
        free(a->data);
        a->data = out;
        a->len = used;
        a->pos = 0;
    }
    return ARCHIVE_OK;
}

int archive_read_next_header(struct archive *a, struct archive_entry **entry) {
    if (a->closed) return ARCHIVE_FATAL;
    int r = next_entry(a, entry);
    if (r < 0) return ARCHIVE_FATAL;
    if (r == 0) return ARCHIVE_EOF;
    return ARCHIVE_OK;
}

ssize_t archive_read_data(struct archive *a, void *buf, size_t n) {
    if (!a->have_entry) return 0;
    struct shim_entry *e = &a->entry;
    size_t remaining = (size_t) (e->size - e->consumed);
    if (n > remaining) n = remaining;
    if (n == 0) return 0;
    memcpy(buf, a->data + a->pos, n);
    a->pos += n;
    e->consumed += (int64_t) n;
    if (a->pos % 512 != 0 && a->pos + (512 - a->pos % 512) <= a->len) {
        /* Data is padded to 512: skip the padding once the entry is consumed. */
        if (e->consumed == e->size) {
            size_t pad = (512 - (size_t) (a->pos % 512)) % 512;
            a->pos += pad;
        }
    }
    return (ssize_t) n;
}

int archive_read_data_into_fd(struct archive *a, int fd) {
    if (!a->have_entry) return ARCHIVE_OK;
    struct shim_entry *e = &a->entry;
    size_t remaining = (size_t) (e->size - e->consumed);
    if (remaining > 0) {
        size_t written = 0;
        while (written < remaining) {
            ssize_t n = write(fd, a->data + a->pos + written, remaining - written);
            if (n <= 0) {
                set_err(a, errno, strerror(errno));
                return ARCHIVE_FATAL;
            }
            written += (size_t) n;
        }
        a->pos += remaining;
        e->consumed = e->size;
    }
    size_t pad = (512 - (size_t) (a->pos % 512)) % 512;
    a->pos += pad;
    return ARCHIVE_OK;
}

int64_t archive_filter_bytes(struct archive *a, int n) {
    (void) n;
    return (int64_t) a->pos;
}

int archive_errno(struct archive *a) { return a->err; }
const char *archive_error_string(struct archive *a) { return a->errmsg[0] ? a->errmsg : NULL; }

int archive_read_free(struct archive *a) {
    if (!a) return ARCHIVE_OK;
    free(a->data);
    if (a->gz) inflateEnd(&a->zs);
    free(a->entry.path); free(a->entry.hardlink); free(a->entry.symlink);
    if (a->out) fclose(a->out);
    free(a->outpath);
    free(a);
    return ARCHIVE_OK;
}

int archive_read_close(struct archive *a) { a->closed = 1; return ARCHIVE_OK; }

/* ---- write API (used by unfakefsify and container export) --------------- */

struct archive *archive_write_new(void) { return calloc(1, sizeof(struct archive)); }

int archive_write_add_filter_gzip(struct archive *a) { a->gz = 1; return ARCHIVE_OK; }
int archive_write_set_format_pax(struct archive *a) { (void) a; return ARCHIVE_OK; }

int archive_write_open_filename(struct archive *a, const char *path) {
    a->is_write = 1;
    a->outpath = strdup(path);
    if (a->gz)
        a->gzw = gzopen(path, "wb");
    else
        a->out = fopen(path, "wb");
    if (!a->gzw && !a->out) {
        set_err(a, errno, strerror(errno));
        return ARCHIVE_FATAL;
    }
    return ARCHIVE_OK;
}

static void put_octal(char *buf, size_t n, int64_t v) {
    char tmp[32];
    snprintf(tmp, sizeof(tmp), "%0*llo", (int) n - 1, (unsigned long long) v);
    memcpy(buf, tmp, n - 1);
}

static int write_raw(struct archive *a, const void *buf, size_t n) {
    if (a->gzw) {
        if (gzwrite(a->gzw, buf, (unsigned) n) != (int) n) {
            set_err(a, EIO, "gzip write failed");
            return -1;
        }
    } else if (fwrite(buf, 1, n, a->out) != n) {
        set_err(a, errno, "write failed");
        return -1;
    }
    return 0;
}

/* Header cache so archive_write_data can encode the size first. */
static struct shim_entry pending;
static int pending_active;
static int64_t pending_written;

int archive_write_header(struct archive *a, struct archive_entry *entry) {
    struct shim_entry *e = (struct shim_entry *) entry;
    struct tar_hdr h;
    memset(&h, 0, sizeof(h));
    snprintf(h.name, sizeof(h.name), "%s", e->path);
    put_octal(h.mode, 8, e->mode);
    put_octal(h.uid, 8, e->uid);
    put_octal(h.gid, 8, e->gid);
    put_octal(h.size, 12, (e->type == S_IFREG || e->type == S_IFLNK) ? 0 : 0);
    put_octal(h.mtime, 12, e->mtime);
    memcpy(h.magic, "ustar", 6);
    memcpy(h.version, "00", 2);
    if (e->type == S_IFDIR) h.typeflag = '5';
    else if (e->type == S_IFLNK) h.typeflag = '2';
    else if (e->hardlink) h.typeflag = '1';
    else if (e->type == S_IFCHR) h.typeflag = '3';
    else if (e->type == S_IFBLK) h.typeflag = '4';
    else if (e->type == S_IFIFO) h.typeflag = '6';
    else h.typeflag = '0';
    if (e->symlink) snprintf(h.linkname, sizeof(h.linkname), "%s", e->symlink);
    if (e->hardlink) snprintf(h.linkname, sizeof(h.linkname), "%s", e->hardlink);
    put_octal(h.devmajor, 8, (e->rdev >> 8) & 0xff);
    put_octal(h.devminor, 8, e->rdev & 0xff);
    memset(h.chksum, ' ', 8);
    unsigned sum = 0;
    for (size_t i = 0; i < sizeof(h); i++)
        sum += ((uint8_t *) &h)[i];
    put_octal(h.chksum, 7, sum);
    h.chksum[7] = ' ';
    /* The size goes in the header before its checksum, so fixed-size entries
     * have to be written through archive_write_data to patch it. */
    if (e->type == S_IFREG) {
        pending = *e;
        pending_active = 1;
        pending_written = 0;
        /* Write the header with the final size when known; callers using this
         * shim always set the size before the header. */
        put_octal(h.size, 12, e->size);
    }
    return write_raw(a, &h, sizeof(h));
}

ssize_t archive_write_data(struct archive *a, const void *buf, size_t n) {
    if (!pending_active) return (ssize_t) n;
    if (pending_written + (int64_t) n > pending.size)
        n = (size_t) (pending.size - pending_written);
    if (n == 0) return 0;
    if (write_raw(a, buf, n) < 0) return -1;
    pending_written += (int64_t) n;
    if (pending_written == pending.size) {
        size_t pad = (512 - (size_t) (pending.size % 512)) % 512;
        static const char zeros[512] = {0};
        if (pad) write_raw(a, zeros, pad);
        pending_active = 0;
    }
    return (ssize_t) n;
}

int archive_write_close(struct archive *a) {
    if (a->gzw) { gzclose(a->gzw); a->gzw = NULL; }
    if (a->out) { fclose(a->out); a->out = NULL; }
    return ARCHIVE_OK;
}

int archive_write_free(struct archive *a) {
    archive_write_close(a);
    free(a->outpath);
    free(a->entry.path); free(a->entry.hardlink); free(a->entry.symlink);
    free(a);
    return ARCHIVE_OK;
}

/* ---- entry objects ----------------------------------------------------- */

struct archive_entry *archive_entry_new(void) {
    return (struct archive_entry *) calloc(1, sizeof(struct shim_entry));
}
void archive_entry_free(struct archive_entry *e) {
    struct shim_entry *s = (struct shim_entry *) e;
    if (!s) return;
    free(s->path); free(s->hardlink); free(s->symlink);
    free(s);
}
#define E(e) ((struct shim_entry *) (e))
const char *archive_entry_pathname(struct archive_entry *e) { return E(e)->path ? E(e)->path : ""; }
void archive_entry_set_pathname(struct archive_entry *e, const char *p) { free(E(e)->path); E(e)->path = strdup(p); }
const char *archive_entry_hardlink(struct archive_entry *e) { return E(e)->hardlink; }
const char *archive_entry_symlink(struct archive_entry *e) { return E(e)->symlink; }
void archive_entry_set_symlink(struct archive_entry *e, const char *p) { free(E(e)->symlink); E(e)->symlink = strdup(p); }
mode_t archive_entry_filetype(struct archive_entry *e) { return E(e)->type; }
void archive_entry_set_mode(struct archive_entry *e, mode_t m) { E(e)->mode = m & 07777; E(e)->type = m & S_IFMT; }
mode_t archive_entry_mode(struct archive_entry *e) { return E(e)->mode | E(e)->type; }
void archive_entry_set_size(struct archive_entry *e, int64_t s) { E(e)->size = s; }
int64_t archive_entry_size(struct archive_entry *e) { return E(e)->size; }
void archive_entry_set_uid(struct archive_entry *e, int64_t v) { E(e)->uid = v; }
void archive_entry_set_gid(struct archive_entry *e, int64_t v) { E(e)->gid = v; }
int64_t archive_entry_uid(struct archive_entry *e) { return E(e)->uid; }
int64_t archive_entry_gid(struct archive_entry *e) { return E(e)->gid; }
void archive_entry_set_rdev(struct archive_entry *e, dev_t v) { E(e)->rdev = (int64_t) v; }
dev_t archive_entry_rdev(struct archive_entry *e) { return (dev_t) E(e)->rdev; }
void archive_entry_set_mtime(struct archive_entry *e, time_t t, long ns) { E(e)->mtime = t; E(e)->mtime_nsec = ns; E(e)->mtime_set = 1; }
void archive_entry_set_atime(struct archive_entry *e, time_t t, long ns) { E(e)->atime = t; E(e)->atime_nsec = ns; E(e)->atime_set = 1; }
void archive_entry_set_ctime(struct archive_entry *e, time_t t, long ns) { E(e)->ctime = t; E(e)->ctime_nsec = ns; }
time_t archive_entry_mtime(struct archive_entry *e) { return E(e)->mtime; }
long archive_entry_mtime_nsec(struct archive_entry *e) { return E(e)->mtime_nsec; }
int archive_entry_mtime_is_set(struct archive_entry *e) { return E(e)->mtime_set; }
time_t archive_entry_atime(struct archive_entry *e) { return E(e)->atime; }
long archive_entry_atime_nsec(struct archive_entry *e) { return E(e)->atime_nsec; }
int archive_entry_atime_is_set(struct archive_entry *e) { return E(e)->atime_set; }

/* ---- hardlink resolver (flat: re-emit hardlinks as they arrive) -------- */

struct archive_entry_linkresolver {
    int unused;
};

struct archive_entry_linkresolver *archive_entry_linkresolver_new(void) {
    return calloc(1, sizeof(struct archive_entry_linkresolver));
}
void archive_entry_linkresolver_set_strategy(struct archive_entry_linkresolver *r, int fmt) { (void) r; (void) fmt; }
void archive_entry_linkresolver_free(struct archive_entry_linkresolver *r) { free(r); }
void archive_entry_linkify(struct archive_entry_linkresolver *r, struct archive_entry **e,
                           struct archive_entry **spare) {
    (void) r;
    /* Every entry is emitted as-is; hardlink targets are already in the
     * archive because tar writes the link after its target. */
    *spare = NULL;
}

/* ---- additional entry accessors used by fakefs_export --------------- */

void archive_entry_set_ino64(struct archive_entry *e, int64_t ino) {
    struct shim_entry *s = E(e);
    (void) ino;
    (void) s;
}
