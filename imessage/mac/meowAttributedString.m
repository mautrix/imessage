// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

#include <Foundation/Foundation.h>
#include "meowAttributedString.h"

id jsonSafeObject(id obj);

NSArray* jsonSafeArray(NSArray* input) {
	NSMutableArray* output = [[NSMutableArray alloc] initWithCapacity:input.count];
	[output enumerateObjectsUsingBlock:^(id object, NSUInteger idx, BOOL *stop) {
		[output setObject:jsonSafeObject(object) atIndexedSubscript:idx];
	}];
	return output;
}

NSDictionary* jsonSafeDict(NSDictionary* input) {
	NSMutableDictionary* output = [[NSMutableDictionary alloc] init];
	[input enumerateKeysAndObjectsUsingBlock:^(id key, id obj, BOOL *stop) {
		[output setObject:jsonSafeObject(obj) forKey:key];
	}];
	return output;
}

id jsonSafeObject(id obj) {
	if ([obj isKindOfClass:[NSString class]] || [obj isKindOfClass:[NSNumber class]] || [obj isKindOfClass:[NSNull class]]) {
		return obj;
	} else if ([obj isKindOfClass:[NSData class]]) {
		return [obj base64EncodedStringWithOptions:0];
	} else if ([obj isKindOfClass:[NSURL class]]) {
		return ((NSURL*)obj).absoluteString;
	} else if ([obj isKindOfClass:[NSDictionary class]]) {
		return jsonSafeDict(obj);
	} else if ([obj isKindOfClass:[NSArray class]]) {
		return jsonSafeArray(obj);
	}
	return @"unknown object";
}

char* meowUnsafeDecodeAttributedString(char* input) {
    NSString* nsInput = @(input);
    NSData* data = [[NSData alloc] initWithBase64EncodedString:nsInput options:0];
    NSUnarchiver* arch = [[NSUnarchiver alloc] initForReadingWithData:data];
    NSAttributedString* str = [arch decodeObject];

    NSMutableArray* attrs = [[NSMutableArray alloc] init];
    [str enumerateAttributesInRange:NSMakeRange(0, [str length]) options:NSAttributedStringEnumerationLongestEffectiveRangeNotRequired usingBlock:
        ^(NSDictionary *attributes, NSRange range, BOOL *stop) {
            [attrs addObject:@{
                @"location": [NSNumber numberWithUnsignedInteger:range.location],
                @"length": [NSNumber numberWithUnsignedInteger:range.length],
                @"values": jsonSafeDict(attributes),
            }];
        }
    ];
    NSDictionary* outputDict = @{
        @"content": str.string,
        @"attributes": attrs,
    };

    NSError *error = NULL;
    NSData *jsonData = [NSJSONSerialization dataWithJSONObject:outputDict options:0 error:&error];
    if (!jsonData && error) {
        NSString* fancyError = [[error.localizedDescription stringByAppendingString:@". "] stringByAppendingString:error.localizedFailureReason];
        return [fancyError UTF8String];
    }
    NSString *jsonString = [[NSString alloc] initWithData:jsonData encoding:NSUTF8StringEncoding];
    return [jsonString UTF8String];
}

char* meowDecodeAttributedString(char* input) {
	@try {
		return meowUnsafeDecodeAttributedString(input);
	}
	@catch (NSException* err) {
		return [[[err.name stringByAppendingString:@": "] stringByAppendingString:err.reason] UTF8String];
	}
}
