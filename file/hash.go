/* A hash table of uint64 key-value pairs. */
package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
)

const (
	HASH_TABLE_GROWTH      = uint64(1048576) // Grows every 1MB
	ENTRY_VALID            = byte(1)
	ENTRY_INVALID          = byte(0)
	ENTRY_SIZE             = uint64(1 + 10 + 10) // byte(validity), uint64(hash key), uint64(value)
	BUCKET_HEADER_SIZE     = uint64(10)          // uint64(next bucket)
	HASH_TABLE_REGION_SIZE = 1024 * 16           // 16KB per locking region
	BUCKET_HEADER_NEW      = uint64(2)           // new bucket will have this header
)

type HashTable struct {
	File                                        *File
	BucketSize, HashBits, PerBucket, NumBuckets uint64
	tableGrowMutex                              sync.Mutex
	regionRWMutex                               []*sync.RWMutex
}

// Open a hash table file.
func OpenHash(name string, hashBits, perBucket uint64) (ht *HashTable, err error) {
	if hashBits < 2 || perBucket < 2 {
		return nil, errors.New(fmt.Sprintf("ERROR: Hash table is too small (%d hash bits, %d per bucket)", hashBits, perBucket))
	}
	file, err := Open(name, HASH_TABLE_GROWTH)
	if err != nil {
		return
	}
	rwMutexes := make([]*sync.RWMutex, file.Size/HASH_TABLE_REGION_SIZE+1)
	for i := range rwMutexes {
		rwMutexes[i] = new(sync.RWMutex)
	}
	ht = &HashTable{File: file, HashBits: hashBits, PerBucket: perBucket,
		tableGrowMutex: sync.Mutex{},
		regionRWMutex:  rwMutexes}
	ht.BucketSize = BUCKET_HEADER_SIZE + ENTRY_SIZE*perBucket
	// file has to be big enough to contain all initial buckets
	if minUsedSize := uint64(math.Pow(2, float64(hashBits))) * ht.BucketSize; ht.File.UsedSize < minUsedSize {
		ht.File.CheckSizeAndEnsure(minUsedSize - ht.File.UsedSize)
		ht.File.UsedSize = minUsedSize
	}
	// move append position to end of final bucket
	if extra := ht.File.UsedSize % ht.BucketSize; extra != 0 {
		ht.File.UsedSize += ht.BucketSize - extra
	}
	ht.NumBuckets = ht.File.UsedSize / ht.BucketSize
	return ht, nil
}

// Return the number of next chained bucket, 0 if there is not any.
func (ht *HashTable) nextBucket(bucket uint64) uint64 {
	if bucket >= ht.NumBuckets {
		return 0
	}
	bucketAddr := bucket * ht.BucketSize
	if next, _ := binary.Uvarint(ht.File.Buf[bucketAddr : bucketAddr+BUCKET_HEADER_SIZE]); next != 0 && next != BUCKET_HEADER_NEW && next <= bucket {
		log.Printf("ERROR: Bucket loop in hash table %s at bucket no.%d, address %d\n", ht.File.Name, bucket, bucketAddr)
		return 0
	} else if next >= ht.NumBuckets {
		return 0
	} else if next == BUCKET_HEADER_NEW {
		return 0
	} else {
		return next
	}
}

// Return the last bucket number in chain.
func (ht *HashTable) lastBucket(bucket uint64) uint64 {
	curr := bucket
	for {
		next := ht.nextBucket(curr)
		if next == 0 {
			return curr
		}
		curr = next
	}
}

// Grow a new bucket on the chain of buckets.
func (ht *HashTable) grow(bucket uint64) {
	// lock both bucket creation and the bucket affected
	ht.tableGrowMutex.Lock()
	// when file is full, we have to lock down everything before growing the file
	if !ht.File.CheckSize(ht.BucketSize) {
		originalMutexes := ht.regionRWMutex
		for _, region := range originalMutexes {
			region.Lock()
		}
		ht.File.CheckSizeAndEnsure(ht.BucketSize)
		// make more mutexes
		moreMutexes := make([]*sync.RWMutex, HASH_TABLE_GROWTH/HASH_TABLE_REGION_SIZE+1)
		for i := range moreMutexes {
			moreMutexes[i] = new(sync.RWMutex)
		}
		// merge mutexes together
		ht.regionRWMutex = append(ht.regionRWMutex, moreMutexes...)
		for _, region := range originalMutexes {
			region.Unlock()
		}
	}
	lastBucketAddr := ht.lastBucket(bucket) * ht.BucketSize
	binary.PutUvarint(ht.File.Buf[lastBucketAddr:lastBucketAddr+10], ht.NumBuckets)
	// mark the new bucket
	newBucketAddr := ht.File.UsedSize
	binary.PutUvarint(ht.File.Buf[newBucketAddr:newBucketAddr+10], BUCKET_HEADER_NEW)
	ht.File.UsedSize += ht.BucketSize
	ht.NumBuckets += 1
	ht.tableGrowMutex.Unlock()
}

// Return a hash key to be used by hash table by masking non-key bits.
func (ht *HashTable) hashKey(key uint64) uint64 {
	return key & ((1 << ht.HashBits) - 1)
}

// Put a new key-value pair.
func (ht *HashTable) Put(key, val uint64) {
	var bucket, entry uint64 = ht.hashKey(key), 0
	region := bucket / HASH_TABLE_REGION_SIZE
	mutex := ht.regionRWMutex[region]
	mutex.Lock()
	for {
		entryAddr := bucket*ht.BucketSize + BUCKET_HEADER_SIZE + entry*ENTRY_SIZE
		if ht.File.Buf[entryAddr] != ENTRY_VALID {
			ht.File.Buf[entryAddr] = ENTRY_VALID
			binary.PutUvarint(ht.File.Buf[entryAddr+1:entryAddr+11], key)
			binary.PutUvarint(ht.File.Buf[entryAddr+11:entryAddr+21], val)
			mutex.Unlock()
			return
		}
		if entry++; entry == ht.PerBucket {
			mutex.Unlock()
			entry = 0
			if bucket = ht.nextBucket(bucket); bucket == 0 {
				ht.grow(ht.hashKey(key))
				ht.Put(key, val)
				return
			}
			region = bucket / HASH_TABLE_REGION_SIZE
			mutex = ht.regionRWMutex[region]
			mutex.Lock()
		}
	}
}

// Get key-value pairs.
func (ht *HashTable) Get(key, limit uint64, filter func(uint64, uint64) bool) (keys, vals []uint64) {
	var count, entry, bucket uint64 = 0, 0, ht.hashKey(key)
	if limit == 0 {
		keys = make([]uint64, 0, 10)
		vals = make([]uint64, 0, 10)
	} else {
		keys = make([]uint64, 0, limit)
		vals = make([]uint64, 0, limit)
	}
	region := bucket / HASH_TABLE_REGION_SIZE
	mutex := ht.regionRWMutex[region]
	mutex.RLock()
	for {
		entryAddr := bucket*ht.BucketSize + BUCKET_HEADER_SIZE + entry*ENTRY_SIZE
		entryKey, _ := binary.Uvarint(ht.File.Buf[entryAddr+1 : entryAddr+11])
		entryVal, _ := binary.Uvarint(ht.File.Buf[entryAddr+11 : entryAddr+21])
		if ht.File.Buf[entryAddr] == ENTRY_VALID {
			if entryKey == key && filter(entryKey, entryVal) {
				keys = append(keys, entryKey)
				vals = append(vals, entryVal)
				if count++; count == limit {
					mutex.RUnlock()
					return
				}
			}
		} else if entryKey == 0 && entryVal == 0 {
			mutex.RUnlock()
			return
		}
		if entry++; entry == ht.PerBucket {
			mutex.RUnlock()
			entry = 0
			if bucket = ht.nextBucket(bucket); bucket == 0 {
				return
			}
			region = bucket / HASH_TABLE_REGION_SIZE
			mutex = ht.regionRWMutex[region]
			mutex.RLock()
		}
	}
}

// Remove specific key-value pair.
func (ht *HashTable) Remove(key, val uint64) {
	var entry, bucket uint64 = 0, ht.hashKey(key)
	region := bucket / HASH_TABLE_REGION_SIZE
	mutex := ht.regionRWMutex[region]
	mutex.Lock()
	for {
		entryAddr := bucket*ht.BucketSize + BUCKET_HEADER_SIZE + entry*ENTRY_SIZE
		entryKey, _ := binary.Uvarint(ht.File.Buf[entryAddr+1 : entryAddr+11])
		entryVal, _ := binary.Uvarint(ht.File.Buf[entryAddr+11 : entryAddr+21])
		if ht.File.Buf[entryAddr] == ENTRY_VALID {
			if entryKey == key && entryVal == val {
				ht.File.Buf[entryAddr] = ENTRY_INVALID
				mutex.Unlock()
				return
			}
		} else if entryKey == 0 && entryVal == 0 {
			mutex.Unlock()
			return
		}
		if entry++; entry == ht.PerBucket {
			mutex.Unlock()
			entry = 0
			if bucket = ht.nextBucket(bucket); bucket == 0 {
				return
			}
			region = bucket / HASH_TABLE_REGION_SIZE
			mutex = ht.regionRWMutex[region]
			mutex.Lock()
		}
	}
}

// Return all entries in the hash table.
func (ht *HashTable) GetAll(limit uint64) (keys, vals []uint64) {
	keys = make([]uint64, 0, 100)
	vals = make([]uint64, 0, 100)
	counter := uint64(0)
	for head := uint64(0); head < uint64(math.Pow(2, float64(ht.HashBits))); head++ {
		var entry, bucket uint64 = 0, head
		region := bucket / HASH_TABLE_REGION_SIZE
		mutex := ht.regionRWMutex[region]
		mutex.RLock()
		for {
			entryAddr := bucket*ht.BucketSize + BUCKET_HEADER_SIZE + entry*ENTRY_SIZE
			entryKey, _ := binary.Uvarint(ht.File.Buf[entryAddr+1 : entryAddr+11])
			entryVal, _ := binary.Uvarint(ht.File.Buf[entryAddr+11 : entryAddr+21])
			if ht.File.Buf[entryAddr] == ENTRY_VALID {
				counter++
				keys = append(keys, entryKey)
				vals = append(vals, entryVal)
				if counter == limit {
					mutex.RUnlock()
					return
				}
			} else if entryKey == 0 && entryVal == 0 {
				mutex.RUnlock()
				break
			}
			if entry++; entry == ht.PerBucket {
				mutex.RUnlock()
				entry = 0
				if bucket = ht.nextBucket(bucket); bucket == 0 {
					return
				}
				region = bucket / HASH_TABLE_REGION_SIZE
				mutex = ht.regionRWMutex[region]
				mutex.RLock()
			}
		}
	}
	return
}
