/* Build-only adapter for local WinFsp DLL experiments, not a product fix. */
#include <windows.h>
#include <stddef.h>
#include <string.h>
/* Older MinGW declares this input non-const; keep its declaration separate. */
#define NPGetUniversalName winfsp_lab_mingw_NPGetUniversalName
#include <npapi.h>
#undef NPGetUniversalName
#define CRED_PACK_GENERIC_CREDENTIALS 0x4
/* MinGW defines this flag but omits the user-mode reparse buffer type.
 * Let WinFsp's existing compatibility declaration provide both. */
#undef SYMLINK_FLAG_RELATIVE
#define STRSAFE_NO_DEPRECATE
#undef FORCEINLINE
#define FORCEINLINE inline __attribute__((always_inline))
#undef __forceinline
#define __forceinline inline __attribute__((always_inline))
#define static_assert _Static_assert
#undef _ReadWriteBarrier
static inline void _ReadWriteBarrier(void) { __asm__ __volatile__("" ::: "memory"); }
static inline void *winfsp_lab_memset(void *, int, size_t);
static inline void *winfsp_lab_memcpy(void *, const void *, size_t);
static inline void *winfsp_lab_memmove(void *, const void *, size_t);
#define memset winfsp_lab_memset
#define memcpy winfsp_lab_memcpy
#define memmove winfsp_lab_memmove
